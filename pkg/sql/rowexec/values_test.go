// Copyright 2017 The Cockroach Authors.
//
// Use of this software is governed by the CockroachDB Software License
// included in the /LICENSE file.

package rowexec

import (
	"context"
	"fmt"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/settings/cluster"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfra"
	"github.com/cockroachdb/cockroach/pkg/sql/execinfrapb"
	"github.com/cockroachdb/cockroach/pkg/sql/randgen"
	"github.com/cockroachdb/cockroach/pkg/sql/rowenc"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/eval"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/types"
	"github.com/cockroachdb/cockroach/pkg/testutils/distsqlutils"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/randutil"
)

func TestValuesProcessor(t *testing.T) {
	defer leaktest.AfterTest(t)()
	defer log.Scope(t).Close(t)

	rng, _ := randutil.NewTestRand()
	for _, numRows := range []int{0, 1, 10, 13, 15} {
		for _, numCols := range []int{0, 1, 3} {
			t.Run(fmt.Sprintf("%d-%d", numRows, numCols), func(t *testing.T) {
				inRows, colTypes := randgen.RandEncDatumRows(rng, numRows, numCols)

				spec, err := execinfra.GenerateValuesSpec(colTypes, inRows)
				if err != nil {
					t.Fatal(err)
				}

				out := &distsqlutils.RowBuffer{}
				st := cluster.MakeTestingClusterSettings()
				evalCtx := eval.NewTestingEvalContext(st)
				defer evalCtx.Stop(context.Background())
				flowCtx := execinfra.FlowCtx{
					Cfg:     &execinfra.ServerConfig{Settings: st},
					EvalCtx: evalCtx,
					Mon:     evalCtx.TestingMon,
				}

				v, err := newValuesProcessor(context.Background(), &flowCtx, 0 /* processorID */, &spec, &execinfrapb.PostProcessSpec{})
				if err != nil {
					t.Fatal(err)
				}
				v.Run(context.Background(), out)
				if !out.ProducerClosed() {
					t.Fatalf("output RowReceiver not closed")
				}

				var res rowenc.EncDatumRows
				for {
					row := out.NextNoMeta(t)
					if row == nil {
						break
					}
					res = append(res, row)
				}

				if len(res) != numRows {
					t.Fatalf("incorrect number of rows %d, expected %d", len(res), numRows)
				}

				var a tree.DatumAlloc
				for i := 0; i < numRows; i++ {
					if len(res[i]) != numCols {
						t.Fatalf("row %d incorrect length %d, expected %d", i, len(res[i]), numCols)
					}
					for j, val := range res[i] {
						cmp, err := val.Compare(context.Background(), colTypes[j], &a, evalCtx, &inRows[i][j])
						if err != nil {
							t.Fatal(err)
						}
						if cmp != 0 {
							t.Errorf(
								"row %d, column %d: received %s, expected %s",
								i, j, val.String(colTypes[j]), inRows[i][j].String(colTypes[j]),
							)
						}
					}
				}
			})
		}
	}
}

func BenchmarkValuesProcessor(b *testing.B) {
	defer log.Scope(b).Close(b)

	ctx := context.Background()
	st := cluster.MakeTestingClusterSettings()
	evalCtx := eval.MakeTestingEvalContext(st)
	defer evalCtx.Stop(ctx)

	flowCtx := execinfra.FlowCtx{
		Cfg:     &execinfra.ServerConfig{Settings: st},
		EvalCtx: &evalCtx,
		Mon:     evalCtx.TestingMon,
	}
	post := execinfrapb.PostProcessSpec{}
	output := rowDisposer{}
	for _, numRows := range []int{1 << 4, 1 << 8, 1 << 12, 1 << 16} {
		for _, numCols := range []int{1, 2, 4} {
			b.Run(fmt.Sprintf("rows=%d,cols=%d", numRows, numCols), func(b *testing.B) {
				typs := types.MakeIntCols(numCols)
				rows := randgen.MakeIntRows(numRows, numCols)
				spec, err := execinfra.GenerateValuesSpec(typs, rows)
				if err != nil {
					b.Fatal(err)
				}

				b.SetBytes(int64(8 * numRows * numCols))
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					v, err := newValuesProcessor(ctx, &flowCtx, 0 /* processorID */, &spec, &post)
					if err != nil {
						b.Fatal(err)
					}
					v.Run(ctx, &output)
				}
			})
		}
	}
}
