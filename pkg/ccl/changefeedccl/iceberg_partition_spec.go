// Copyright 2025 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package changefeedccl

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cockroachdb/errors"
)

// PartitionSpec is a minimal in-memory representation of an Iceberg partition spec
// sufficient to evaluate transforms for partition path construction.
type PartitionSpec struct {
	SpecID int
	Fields []PartitionField
}

// PartitionField represents a single partition transform on a source column.
// Name is the partition field name. Source is the source column name.
// Transform encodes transform and parameter, e.g. "identity", "bucket[16]", "truncate[8]",
// "year", "month", "day", "hour".
type PartitionField struct {
	Name      string
	Source    string
	Transform string
}

// parsePartitionSpecJSON parses a JSON spec passed via sink config. It supports either
// {"spec_id":N, "fields":[...]} or {"spec":{...}} forms.
func parsePartitionSpecJSON(raw []byte, explicitID int) (PartitionSpec, int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return PartitionSpec{}, 0, errors.Wrap(err, "unmarshal partition spec")
	}
	var body json.RawMessage
	if v, ok := top["spec"]; ok && len(v) > 0 {
		body = v
	} else {
		body = raw
	}
	// target struct
	var tmp struct {
		SpecID int `json:"spec_id"`
		Fields []struct {
			Name      string `json:"name"`
			Source    string `json:"source"`
			Transform string `json:"transform"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(body, &tmp); err != nil {
		return PartitionSpec{}, 0, errors.Wrap(err, "unmarshal partition spec body")
	}
	spec := PartitionSpec{SpecID: tmp.SpecID}
	for _, f := range tmp.Fields {
		if f.Name == "" {
			f.Name = f.Source
		}
		spec.Fields = append(spec.Fields, PartitionField{Name: f.Name, Source: f.Source, Transform: strings.ToLower(f.Transform)})
	}
	id := spec.SpecID
	if id == 0 && explicitID != 0 {
		id = explicitID
	}
	if len(spec.Fields) == 0 {
		return spec, id, nil
	}
	for _, f := range spec.Fields {
		if f.Source == "" || f.Transform == "" {
			return PartitionSpec{}, 0, errors.Newf("invalid partition field: %+v", f)
		}
		// Basic validation of transform name
		switch {
		case f.Transform == "identity", f.Transform == "year", f.Transform == "month", f.Transform == "day", f.Transform == "hour":
		case strings.HasPrefix(f.Transform, "bucket[") && strings.HasSuffix(f.Transform, "]"):
		case strings.HasPrefix(f.Transform, "truncate[") && strings.HasSuffix(f.Transform, "]"):
		default:
			return PartitionSpec{}, 0, errors.Newf("unsupported transform %q", f.Transform)
		}
	}
	return spec, id, nil
}

func (p PartitionSpec) String() string {
	parts := make([]string, 0, len(p.Fields))
	for _, f := range p.Fields {
		parts = append(parts, fmt.Sprintf("%s=%s(%s)", f.Name, f.Transform, f.Source))
	}
	return fmt.Sprintf("spec_id=%d fields=[%s]", p.SpecID, strings.Join(parts, ","))
}
