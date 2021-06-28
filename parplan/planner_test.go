package parplan

import (
	"context"
	"github.com/squareup/pranadb"
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/storage"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPlanner(t *testing.T) {

	pr := NewParser()

	stmtNode, err := pr.Parse("select a, b from test.table1 where b > 3 and b = (select max(b) from test.table1)")

	require.Nil(t, err)

	pl := NewPlanner()

	prana, err := CreatePrana()
	require.Nil(t, err)

	is, err := prana.GetInfoSchema()
	require.Nil(t, err)

	ctx := context.TODO()
	sessCtx := NewSessionContext(is)

	logical, err := pl.CreateLogicalPlan(ctx, sessCtx, stmtNode, is)
	require.Nil(t, err)

	physical, err := pl.CreatePhysicalPlan(ctx, sessCtx, logical, true, false)
	require.Nil(t, err)

	println(physical.ExplainInfo())
}

func CreatePrana() (*pranadb.PranaNode, error) {
	tableInfo := common.TableInfo{}
	tableInfo.
		Id(0).
		Name("table1").
		AddColumn("a", common.VarcharColumnType).
		AddColumn("b", common.BigIntColumnType).
		AddColumn("c", common.BigIntColumnType)

	nodeID := 1
	stor := storage.NewFakeStorage(nodeID, 10)
	prana, err := pranadb.NewPranaNode(stor, nodeID)
	if err != nil {
		return nil, err
	}
	colNames := []string{"a", "b", "c"}
	colTypes := []common.ColumnType{common.VarcharColumnType, common.BigIntColumnType, common.IntColumnType}
	err = prana.CreateSource("test", "source1", colNames, colTypes, nil, nil)
	if err != nil {
		return nil, err
	}
	return prana, nil
}
