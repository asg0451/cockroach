// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/cockroach/pkg/ccl/changefeedccl/cdcevent"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/spaolacci/murmur3"
)

// EvaluatePartition computes partition values for the given row using the provided spec.
// Returns a name->string map for each field in spec.
func EvaluatePartition(row cdcevent.Row, spec PartitionSpec) (map[string]string, error) {
	if len(spec.Fields) == 0 {
		return nil, nil
	}
	// Build a simple name->(datum,type) map from the row.
	values := map[string]struct {
		d tree.Datum
		t *types.T
	}{}
	if err := row.ForAllColumns().Datum(func(d tree.Datum, col cdcevent.ResultColumn) error {
		values[strings.ToLower(col.Name)] = struct {
			d tree.Datum
			t *types.T
		}{d: d, t: col.Typ}
		return nil
	}); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(spec.Fields))
	for _, f := range spec.Fields {
		src := strings.ToLower(f.Source)
		v, ok := values[src]
		if !ok {
			return nil, errors.Newf("partition source column %q not found", f.Source)
		}
		sval, err := evalTransform(v.d, v.t, f.Transform)
		if err != nil {
			return nil, errors.Wrapf(err, "eval transform %s on %s", f.Transform, f.Source)
		}
		out[f.Name] = sval
	}
	return out, nil
}

func evalTransform(d tree.Datum, t *types.T, transform string) (string, error) {
	tr := transform
	switch {
	case tr == "identity":
		return datumToString(d)
	case tr == "year", tr == "month", tr == "day", tr == "hour":
		// Works for timestamp/timestamptz/date
		tm, err := datumToTime(d, t)
		if err != nil {
			return "", err
		}
		y, m, day := tm.Date()
		switch tr {
		case "year":
			return strconv.Itoa(y), nil
		case "month":
			return fmt.Sprintf("%04d-%02d", y, int(m)), nil
		case "day":
			return fmt.Sprintf("%04d-%02d-%02d", y, int(m), day), nil
		case "hour":
			return tm.UTC().Format("2006-01-02-15"), nil
		}
	case strings.HasPrefix(tr, "bucket[") && strings.HasSuffix(tr, "]"):
		nstr := strings.TrimSuffix(strings.TrimPrefix(tr, "bucket["), "]")
		n, err := strconv.Atoi(nstr)
		if err != nil || n <= 0 {
			return "", errors.Newf("invalid bucket param %q", nstr)
		}
		// Hash the identity string value using murmur3 32-bit, mod n.
		s, err := datumToString(d)
		if err != nil {
			return "", err
		}
		h := murmur3.New32()
		_, _ = h.Write([]byte(s))
		return strconv.Itoa(int(h.Sum32() % uint32(n))), nil
	case strings.HasPrefix(tr, "truncate[") && strings.HasSuffix(tr, "]"):
		nstr := strings.TrimSuffix(strings.TrimPrefix(tr, "truncate["), "]")
		n, err := strconv.Atoi(nstr)
		if err != nil || n <= 0 {
			return "", errors.Newf("invalid truncate param %q", nstr)
		}
		// For ints and decimals, floor to nearest multiple.
		switch t.Family() {
		case types.IntFamily:
			v := int64(tree.MustBeDInt(d))
			base := (v / int64(n)) * int64(n)
			return strconv.FormatInt(base, 10), nil
		case types.DecimalFamily:
			// Use string identity; truncate width in characters.
			s, err := datumToString(d)
			if err != nil {
				return "", err
			}
			if len(s) > n {
				s = s[:n]
			}
			return s, nil
		case types.BytesFamily, types.StringFamily:
			s, err := datumToString(d)
			if err != nil {
				return "", err
			}
			if len(s) > n {
				s = s[:n]
			}
			return s, nil
		default:
			return "", errors.Newf("truncate not supported for type %s", t)
		}
	}
	return "", errors.Newf("unsupported transform %q", transform)
}

func datumToTime(d tree.Datum, t *types.T) (time.Time, error) {
	switch t.Family() {
	case types.TimestampFamily:
		ts := tree.MustBeDTimestamp(d)
		return ts.Time, nil
	case types.TimestampTZFamily:
		ts := tree.MustBeDTimestampTZ(d)
		return ts.Time, nil
	case types.DateFamily:
		dd := tree.MustBeDDate(d)
		// Convert to midnight UTC for partitioning
		return timeutil.Unix(0, 0).AddDate(0, 0, int(dd.PGEpochDays())), nil
	default:
		return time.Time{}, errors.Newf("cannot derive time from type %s", t)
	}
}

func datumToString(d tree.Datum) (string, error) {
	switch v := d.(type) {
	case *tree.DString:
		return string(*v), nil
	case *tree.DBytes:
		return string(*v), nil
	case *tree.DInt:
		return strconv.FormatInt(int64(*v), 10), nil
	case *tree.DDecimal:
		return v.Decimal.String(), nil
	case *tree.DTimestamp:
		return v.Time.UTC().Format(time.RFC3339Nano), nil
	case *tree.DTimestampTZ:
		return v.Time.UTC().Format(time.RFC3339Nano), nil
	case *tree.DDate:
		// Format as YYYY-MM-DD
		t, _ := datumToTime(v, types.Date)
		y, m, d := t.Date()
		return fmt.Sprintf("%04d-%02d-%02d", y, int(m), d), nil
	default:
		return "", errors.Newf("unsupported datum type %T for identity", d)
	}
}
