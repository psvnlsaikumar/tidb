// Copyright 2019 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package core

import (
	"bytes"
	"crypto/sha256"
	"hash"
	"strconv"
	"sync"

	"github.com/pingcap/failpoint"
	"github.com/pingcap/tidb/kv"
	"github.com/pingcap/tidb/parser"
	"github.com/pingcap/tidb/util/plancodec"
)

// EncodeFlatPlan encodes a FlatPhysicalPlan with compression.
func EncodeFlatPlan(flat *FlatPhysicalPlan) string {
	if len(flat.Main) == 0 {
		return ""
	}
	// We won't collect the plan when we're in "EXPLAIN FOR" statement and the plan is from EXECUTE statement (please
	// read comments of InExecute for details about the meaning of InExecute) because we are unable to get some
	// necessary information when the execution of the plan is finished and some states in the session such as
	// PreparedParams are cleaned.
	// The behavior in BinaryPlanStrFromFlatPlan() is also the same.
	if flat.InExecute {
		return ""
	}
	failpoint.Inject("mockPlanRowCount", func(val failpoint.Value) {
		selectPlan := flat.Main.GetSelectPlan()
		for _, op := range selectPlan {
			op.Origin.statsInfo().RowCount = float64(val.(int))
		}
	})
	pn := encoderPool.Get().(*planEncoder)
	defer func() {
		pn.buf.Reset()
		encoderPool.Put(pn)
	}()
	buf := pn.buf
	buf.Reset()
	opCount := len(flat.Main)
	for _, cte := range flat.CTEs {
		opCount += len(cte)
	}
	// assume an operator costs around 80 bytes, preallocate space for them
	buf.Grow(80 * opCount)
	encodeFlatPlanTree(flat.Main, 0, &buf)
	for _, cte := range flat.CTEs {
		op := cte[0]
		cteDef := cte[0].Origin.(*CTEDefinition)
		id := cteDef.CTE.IDForStorage
		tp := plancodec.TypeCTEDefinition
		taskTypeInfo := plancodec.EncodeTaskType(op.IsRoot, op.StoreType)
		p := op.Origin
		actRows, analyzeInfo, memoryInfo, diskInfo := getRuntimeInfoStr(p.SCtx(), p, nil)
		var estRows float64
		if statsInfo := p.statsInfo(); statsInfo != nil {
			estRows = statsInfo.RowCount
		}
		plancodec.EncodePlanNode(
			int(op.Depth),
			strconv.Itoa(id)+op.Label.String(),
			tp,
			estRows,
			taskTypeInfo,
			op.Origin.ExplainInfo(),
			actRows,
			analyzeInfo,
			memoryInfo,
			diskInfo,
			&buf,
		)
		if len(cte) > 1 {
			encodeFlatPlanTree(cte[1:], 1, &buf)
		}
	}
	return plancodec.Compress(buf.Bytes())
}

func encodeFlatPlanTree(flatTree FlatPlanTree, offset int, buf *bytes.Buffer) {
	for _, op := range flatTree {
		taskTypeInfo := plancodec.EncodeTaskType(op.IsRoot, op.StoreType)
		p := op.Origin
		actRows, analyzeInfo, memoryInfo, diskInfo := getRuntimeInfoStr(p.SCtx(), p, nil)
		var estRows float64
		if statsInfo := p.statsInfo(); statsInfo != nil {
			estRows = statsInfo.RowCount
		}
		plancodec.EncodePlanNode(
			int(op.Depth),
			strconv.Itoa(op.Origin.ID())+op.Label.String(),
			op.Origin.TP(),
			estRows,
			taskTypeInfo,
			op.Origin.ExplainInfo(),
			actRows,
			analyzeInfo,
			memoryInfo,
			diskInfo,
			buf,
		)

		// If NeedReverseDriverSide is true, we stop using the order of the slice and switch to recursively
		// call encodeFlatPlanTree to keep build side before probe side.
		if op.NeedReverseDriverSide {
			buildSide := flatTree[op.ChildrenIdx[1]-offset:]
			probeSide := flatTree[op.ChildrenIdx[0]-offset : op.ChildrenIdx[1]-offset]
			encodeFlatPlanTree(buildSide, op.ChildrenIdx[1], buf)
			encodeFlatPlanTree(probeSide, op.ChildrenIdx[0], buf)
			break
		}
	}
}

var encoderPool = sync.Pool{
	New: func() interface{} {
		return &planEncoder{}
	},
}

type planEncoder struct {
	buf          bytes.Buffer
	encodedPlans map[int]bool

	ctes []*PhysicalCTE
}

// EncodePlan is used to encodePlan the plan to the plan tree with compressing.
// Deprecated: FlattenPhysicalPlan() + EncodeFlatPlan() is preferred.
func EncodePlan(p Plan) string {
	if explain, ok := p.(*Explain); ok {
		p = explain.TargetPlan
	}
	if p == nil || p.SCtx() == nil {
		return ""
	}
	pn := encoderPool.Get().(*planEncoder)
	defer encoderPool.Put(pn)
	selectPlan := getSelectPlan(p)
	if selectPlan != nil {
		failpoint.Inject("mockPlanRowCount", func(val failpoint.Value) {
			selectPlan.statsInfo().RowCount = float64(val.(int))
		})
	}
	return pn.encodePlanTree(p)
}

func (pn *planEncoder) encodePlanTree(p Plan) string {
	pn.encodedPlans = make(map[int]bool)
	pn.buf.Reset()
	pn.ctes = pn.ctes[:0]
	pn.encodePlan(p, true, kv.TiKV, 0)
	pn.encodeCTEPlan()
	return plancodec.Compress(pn.buf.Bytes())
}

func (pn *planEncoder) encodeCTEPlan() {
	if len(pn.ctes) <= 0 {
		return
	}
	explainedCTEPlan := make(map[int]struct{})
	for i := 0; i < len(pn.ctes); i++ {
		x := (*CTEDefinition)(pn.ctes[i])
		// skip if the CTE has been explained, the same CTE has same IDForStorage
		if _, ok := explainedCTEPlan[x.CTE.IDForStorage]; ok {
			continue
		}
		taskTypeInfo := plancodec.EncodeTaskType(true, kv.TiKV)
		actRows, analyzeInfo, memoryInfo, diskInfo := getRuntimeInfoStr(x.SCtx(), x, nil)
		rowCount := 0.0
		if statsInfo := x.statsInfo(); statsInfo != nil {
			rowCount = x.statsInfo().RowCount
		}
		plancodec.EncodePlanNode(0, strconv.Itoa(x.CTE.IDForStorage), plancodec.TypeCTEDefinition, rowCount, taskTypeInfo, x.ExplainInfo(), actRows, analyzeInfo, memoryInfo, diskInfo, &pn.buf)
		pn.encodePlan(x.SeedPlan, true, kv.TiKV, 1)
		if x.RecurPlan != nil {
			pn.encodePlan(x.RecurPlan, true, kv.TiKV, 1)
		}
		explainedCTEPlan[x.CTE.IDForStorage] = struct{}{}
	}
}

func (pn *planEncoder) encodePlan(p Plan, isRoot bool, store kv.StoreType, depth int) {
	taskTypeInfo := plancodec.EncodeTaskType(isRoot, store)
	actRows, analyzeInfo, memoryInfo, diskInfo := getRuntimeInfoStr(p.SCtx(), p, nil)
	rowCount := 0.0
	if statsInfo := p.statsInfo(); statsInfo != nil {
		rowCount = p.statsInfo().RowCount
	}
	plancodec.EncodePlanNode(depth, strconv.Itoa(p.ID()), p.TP(), rowCount, taskTypeInfo, p.ExplainInfo(), actRows, analyzeInfo, memoryInfo, diskInfo, &pn.buf)
	pn.encodedPlans[p.ID()] = true
	depth++

	selectPlan := getSelectPlan(p)
	if selectPlan == nil {
		return
	}
	if !pn.encodedPlans[selectPlan.ID()] {
		pn.encodePlan(selectPlan, isRoot, store, depth)
		return
	}
	for _, child := range selectPlan.Children() {
		if pn.encodedPlans[child.ID()] {
			continue
		}
		pn.encodePlan(child, isRoot, store, depth)
	}
	switch copPlan := selectPlan.(type) {
	case *PhysicalTableReader:
		pn.encodePlan(copPlan.tablePlan, false, copPlan.StoreType, depth)
	case *PhysicalIndexReader:
		pn.encodePlan(copPlan.indexPlan, false, store, depth)
	case *PhysicalIndexLookUpReader:
		pn.encodePlan(copPlan.indexPlan, false, store, depth)
		pn.encodePlan(copPlan.tablePlan, false, store, depth)
	case *PhysicalIndexMergeReader:
		for _, p := range copPlan.partialPlans {
			pn.encodePlan(p, false, store, depth)
		}
		if copPlan.tablePlan != nil {
			pn.encodePlan(copPlan.tablePlan, false, store, depth)
		}
	case *PhysicalCTE:
		pn.ctes = append(pn.ctes, copPlan)
	}
}

var digesterPool = sync.Pool{
	New: func() interface{} {
		return &planDigester{
			hasher: sha256.New(),
		}
	},
}

type planDigester struct {
	buf          bytes.Buffer
	encodedPlans map[int]bool
	hasher       hash.Hash
}

// NormalizeFlatPlan normalizes a FlatPhysicalPlan and generates plan digest.
func NormalizeFlatPlan(flat *FlatPhysicalPlan) (normalized string, digest *parser.Digest) {
	selectPlan := flat.Main.GetSelectPlan()
	if len(selectPlan) == 0 || !selectPlan[0].IsPhysicalPlan {
		return "", parser.NewDigest(nil)
	}
	d := digesterPool.Get().(*planDigester)
	defer func() {
		d.buf.Reset()
		d.hasher.Reset()
		digesterPool.Put(d)
	}()
	// assume an operator costs around 30 bytes, preallocate space for them
	d.buf.Grow(30 * len(selectPlan))
	depthOffset := len(flat.Main) - len(selectPlan)
	for _, op := range selectPlan {
		taskTypeInfo := plancodec.EncodeTaskTypeForNormalize(op.IsRoot, op.StoreType)
		p := op.Origin.(PhysicalPlan)
		plancodec.NormalizePlanNode(
			int(op.Depth-uint32(depthOffset)),
			op.Origin.TP(),
			taskTypeInfo,
			p.ExplainNormalizedInfo(),
			&d.buf,
		)
	}
	normalized = d.buf.String()
	_, err := d.hasher.Write(d.buf.Bytes())
	if err != nil {
		panic(err)
	}
	digest = parser.NewDigest(d.hasher.Sum(nil))
	return
}

// NormalizePlan is used to normalize the plan and generate plan digest.
// Deprecated: FlattenPhysicalPlan() + NormalizeFlatPlan() is preferred.
func NormalizePlan(p Plan) (normalized string, digest *parser.Digest) {
	selectPlan := getSelectPlan(p)
	if selectPlan == nil {
		return "", parser.NewDigest(nil)
	}
	d := digesterPool.Get().(*planDigester)
	defer func() {
		d.buf.Reset()
		d.hasher.Reset()
		digesterPool.Put(d)
	}()
	d.normalizePlanTree(selectPlan)
	normalized = d.buf.String()
	_, err := d.hasher.Write(d.buf.Bytes())
	if err != nil {
		panic(err)
	}
	digest = parser.NewDigest(d.hasher.Sum(nil))
	return
}

func (d *planDigester) normalizePlanTree(p PhysicalPlan) {
	d.encodedPlans = make(map[int]bool)
	d.buf.Reset()
	d.normalizePlan(p, true, kv.TiKV, 0)
}

func (d *planDigester) normalizePlan(p PhysicalPlan, isRoot bool, store kv.StoreType, depth int) {
	taskTypeInfo := plancodec.EncodeTaskTypeForNormalize(isRoot, store)
	plancodec.NormalizePlanNode(depth, p.TP(), taskTypeInfo, p.ExplainNormalizedInfo(), &d.buf)
	d.encodedPlans[p.ID()] = true

	depth++
	for _, child := range p.Children() {
		if d.encodedPlans[child.ID()] {
			continue
		}
		d.normalizePlan(child, isRoot, store, depth)
	}
	switch x := p.(type) {
	case *PhysicalTableReader:
		d.normalizePlan(x.tablePlan, false, x.StoreType, depth)
	case *PhysicalIndexReader:
		d.normalizePlan(x.indexPlan, false, store, depth)
	case *PhysicalIndexLookUpReader:
		d.normalizePlan(x.indexPlan, false, store, depth)
		d.normalizePlan(x.tablePlan, false, store, depth)
	case *PhysicalIndexMergeReader:
		for _, p := range x.partialPlans {
			d.normalizePlan(p, false, store, depth)
		}
		if x.tablePlan != nil {
			d.normalizePlan(x.tablePlan, false, store, depth)
		}
	}
}

func getSelectPlan(p Plan) PhysicalPlan {
	var selectPlan PhysicalPlan
	if physicalPlan, ok := p.(PhysicalPlan); ok {
		selectPlan = physicalPlan
	} else {
		switch x := p.(type) {
		case *Delete:
			selectPlan = x.SelectPlan
		case *Update:
			selectPlan = x.SelectPlan
		case *Insert:
			selectPlan = x.SelectPlan
		case *Explain:
			selectPlan = getSelectPlan(x.TargetPlan)
		}
	}
	return selectPlan
}
