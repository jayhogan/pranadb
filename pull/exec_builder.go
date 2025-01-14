package pull

import (
	"fmt"
	"strings"

	"github.com/pingcap/parser/model"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/errors"
	"github.com/squareup/pranadb/pull/exec"
	"github.com/squareup/pranadb/sess"
	"github.com/squareup/pranadb/sharder"
	"github.com/squareup/pranadb/tidb/planner"
	"github.com/squareup/pranadb/tidb/planner/util"
	"github.com/squareup/pranadb/tidb/util/ranger"
)

// nolint: gocyclo
func (p *Engine) buildPullDAG(session *sess.Session, plan planner.PhysicalPlan, remote bool) (exec.PullExecutor, error) {
	cols := plan.Schema().Columns
	colTypes := make([]common.ColumnType, 0, len(cols))
	colNames := make([]string, 0, len(cols))
	for _, col := range cols {
		colType := col.GetType()
		pranaType := common.ConvertTiDBTypeToPranaType(colType)
		colTypes = append(colTypes, pranaType)
		colNames = append(colNames, col.OrigName)
	}
	var executor exec.PullExecutor
	var err error
	switch op := plan.(type) {
	case *planner.PhysicalProjection:
		var exprs []*common.Expression
		for _, expr := range op.Exprs {
			exprs = append(exprs, common.NewExpression(expr))
		}
		executor, err = exec.NewPullProjection(colNames, colTypes, exprs)
		if err != nil {
			return nil, errors.WithStack(err)
		}
	case *planner.PhysicalSelection:
		var exprs []*common.Expression
		for _, expr := range op.Conditions {
			exprs = append(exprs, common.NewExpression(expr))
		}
		executor = exec.NewPullSelect(colNames, colTypes, exprs)
	case *planner.PhysicalTableScan:
		if remote {
			tableName := op.Table.Name.L
			executor, err = p.createPullTableScan(session.Schema, tableName, op.Ranges, op.Columns, session.QueryInfo.ShardID)
			if err != nil {
				return nil, errors.WithStack(err)
			}
		} else {
			remoteDag, err := p.buildPullDAG(session, op, true)
			if err != nil {
				return nil, errors.WithStack(err)
			}
			pointGetShardID, err := p.getPointGetShardID(session, op.Ranges, op.Table.Name.L)
			if err != nil {
				return nil, err
			}
			executor = exec.NewRemoteExecutor(remoteDag, session.QueryInfo, colNames, colTypes, session.Schema.Name, p.cluster,
				pointGetShardID)
		}
	case *planner.PhysicalIndexScan:
		if remote {
			tableName := op.Table.Name.L
			if op.Index.Primary {
				// This is a fake index we created because the table has a composite PK and TiDB planner doesn't
				// support this case well. Having a fake index allows the planner to create multiple ranges for fast
				// scans and lookup for the composite PK case
				executor, err = p.createPullTableScan(session.Schema, tableName, op.Ranges, op.Columns, session.QueryInfo.ShardID)
				if err != nil {
					return nil, errors.WithStack(err)
				}
			} else {
				indexName := op.Index.Name.L
				executor, err = p.createPullIndexScan(session.Schema, tableName, indexName, op.Ranges, op.Columns, session.QueryInfo.ShardID)
				if err != nil {
					return nil, errors.WithStack(err)
				}
			}
		} else {
			remoteDag, err := p.buildPullDAG(session, op, true)
			if err != nil {
				return nil, err
			}
			executor = exec.NewRemoteExecutor(remoteDag, session.QueryInfo, colNames, colTypes, session.Schema.Name, p.cluster,
				-1)
		}
	case *planner.PhysicalSort:
		desc, sortByExprs := p.byItemsToDescAndSortExpression(op.ByItems)
		executor = exec.NewPullSort(colNames, colTypes, desc, sortByExprs)
	case *planner.PhysicalLimit:
		executor = exec.NewPullLimit(colNames, colTypes, op.Count, op.Offset)
	case *planner.PhysicalTopN:
		limit := exec.NewPullLimit(colNames, colTypes, op.Count, op.Offset)
		desc, sortByExprs := p.byItemsToDescAndSortExpression(op.ByItems)
		sort := exec.NewPullSort(colNames, colTypes, desc, sortByExprs)
		executor = exec.NewPullChain(limit, sort)
	default:
		return nil, errors.Errorf("unexpected plan type %T", plan)
	}

	var childExecutors []exec.PullExecutor
	for _, child := range plan.Children() {
		childExecutor, err := p.buildPullDAG(session, child, remote)
		if err != nil {
			return nil, errors.WithStack(err)
		}
		if childExecutor != nil {
			childExecutors = append(childExecutors, childExecutor)
		}
	}
	exec.ConnectPullExecutors(childExecutors, executor)

	return executor, nil
}

func (p *Engine) getPointGetShardID(session *sess.Session, ranges []*ranger.Range, tableName string) (int64, error) {
	var pointGetShardID int64 = -1
	if len(ranges) == 1 {
		rng := ranges[0]
		if rng.IsPoint(session.Planner().StatementContext()) {
			if len(rng.LowVal) != 1 {
				// Composite ranges not supported yet
				return -1, nil
			}
			table, ok := session.Schema.GetTable(tableName)
			if !ok {
				return 0, errors.Errorf("cannot find table %s", tableName)
			}
			if table.GetTableInfo().ColumnTypes[table.GetTableInfo().PrimaryKeyCols[0]].Type == common.TypeDecimal {
				// We don't currently support optimised point gets for keys of type Decimal
				return -1, nil
			}
			v := common.TiDBValueToPranaValue(rng.LowVal[0].GetValue())
			k := []interface{}{v}

			key, err := common.EncodeKey(k, table.GetTableInfo().ColumnTypes, table.GetTableInfo().PrimaryKeyCols, []byte{})
			if err != nil {
				return 0, err
			}
			pgsid, err := p.shrder.CalculateShard(sharder.ShardTypeHash, key)
			if err != nil {
				return 0, err
			}
			pointGetShardID = int64(pgsid)
		}
	}
	return pointGetShardID, nil
}

func (p *Engine) createPullTableScan(schema *common.Schema, tableName string, ranges []*ranger.Range, columns []*model.ColumnInfo, shardID uint64) (exec.PullExecutor, error) {
	tbl, ok := schema.GetTable(tableName)
	if !ok {
		return nil, errors.Errorf("unknown source or materialized view %s", tableName)
	}
	scanRanges := createScanRanges(ranges)
	var colIndexes []int
	for _, col := range columns {
		colIndexes = append(colIndexes, col.Offset)
	}
	return exec.NewPullTableScan(tbl.GetTableInfo(), colIndexes, p.cluster, shardID, scanRanges)
}

func (p *Engine) createPullIndexScan(schema *common.Schema, tableName string, indexName string, ranges []*ranger.Range,
	columnInfos []*model.ColumnInfo, shardID uint64) (exec.PullExecutor, error) {
	tbl, ok := schema.GetTable(tableName)
	if !ok {
		return nil, errors.Errorf("unknown source or materialized view %s", tableName)
	}
	idx, ok := tbl.GetTableInfo().IndexInfos[strings.Replace(indexName, tableName+"_u", "", 1)]
	if !ok {
		return nil, errors.Errorf("unknown index %s", indexName)
	}
	scanRanges := createScanRanges(ranges)
	var colIndexes []int
	for _, colInfo := range columnInfos {
		colIndexes = append(colIndexes, colInfo.Offset)
	}
	return exec.NewPullIndexReader(tbl.GetTableInfo(), idx, colIndexes, p.cluster, shardID, scanRanges)
}

func createScanRanges(ranges []*ranger.Range) []*exec.ScanRange {
	scanRanges := make([]*exec.ScanRange, len(ranges))
	for i, rng := range ranges {
		if !rng.IsFullRange() {
			nr := len(rng.LowVal)
			lowVals := make([]interface{}, nr)
			highVals := make([]interface{}, nr)
			for j := 0; j < len(rng.LowVal); j++ {
				lowD := rng.LowVal[j]
				highD := rng.HighVal[j]
				lowVals[j] = common.TiDBValueToPranaValue(lowD.GetValue())
				highVals[j] = common.TiDBValueToPranaValue(highD.GetValue())
			}
			scanRanges[i] = &exec.ScanRange{
				LowVals:  lowVals,
				HighVals: highVals,
				LowExcl:  rng.LowExclude,
				HighExcl: rng.HighExclude,
			}
		}
	}
	return scanRanges
}

func (p *Engine) byItemsToDescAndSortExpression(byItems []*util.ByItems) ([]bool, []*common.Expression) {
	lbi := len(byItems)
	desc := make([]bool, lbi)
	sortByExprs := make([]*common.Expression, lbi)
	for i, byitem := range byItems {
		desc[i] = byitem.Desc
		sortByExprs[i] = common.NewExpression(byitem.Expr)
	}
	return desc, sortByExprs
}

func dumpPhysicalPlan(plan planner.PhysicalPlan) string { // nolint: deadcode
	builder := &strings.Builder{}
	dumpPhysicalPlanRec(plan, 0, builder)
	return builder.String()
}

func dumpPhysicalPlanRec(plan planner.PhysicalPlan, level int, builder *strings.Builder) {
	for i := 0; i < level-1; i++ {
		builder.WriteString("   |")
	}
	if level > 0 {
		builder.WriteString("   > ")
	}
	builder.WriteString(fmt.Sprintf("%T\n", plan))
	for _, child := range plan.Children() {
		dumpPhysicalPlanRec(child, level+1, builder)
	}
}

func dumpPullDAG(pullDAG exec.PullExecutor) string { // nolint: deadcode
	builder := &strings.Builder{}
	dumpPullDAGRec(pullDAG, 0, builder)
	return builder.String()
}

func dumpPullDAGRec(pullDAG exec.PullExecutor, level int, builder *strings.Builder) {
	for i := 0; i < level-1; i++ {
		builder.WriteString("   |")
	}
	if level > 0 {
		builder.WriteString("   > ")
	}
	builder.WriteString(fmt.Sprintf("%T\n", pullDAG))
	for _, child := range pullDAG.GetChildren() {
		dumpPullDAGRec(child, level+1, builder)
	}
}
