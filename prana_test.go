package pranadb

import (
	"github.com/squareup/pranadb/common"
	"github.com/squareup/pranadb/storage"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestCreateMaterializedView(t *testing.T) {
	store := storage.NewFakeStorage(1, 10)
	prana, err := NewPranaNode(store, 1)
	require.Nil(t, err)

	colTypes := []common.ColumnType{common.TypeBigInt, common.TypeVarchar, common.TypeDouble}

	err = prana.CreateSource("test", "sensor_readings", []string{"sensor_id", "location", "temperature"}, colTypes, []int{0}, nil)
	require.Nil(t, err)

	query := "select sensor_id, max(temperature) from test.sensor_readings where location='wincanton' group by sensor_id"
	err = prana.CreateMaterializedView("test", "max_readings", query)
	require.Nil(t, err)

	rf, err := common.NewRowsFactory(colTypes)
	require.Nil(t, err)

	rows := rf.NewRows(10)

	appendRow(t, rows, colTypes, 1, "wincanton", 25.5)
	appendRow(t, rows, colTypes, 2, "london", 28.1)
	appendRow(t, rows, colTypes, 3, "los angeles", 35.6)

	source, ok := prana.getSource("test", "sensor_readings")
	require.True(t, ok)

	err = source.IngestRows(rows, 1)
	require.Nil(t, err)

	time.Sleep(5 * time.Second)

	mv, ok := prana.getMaterializedView("test", "max_readings")
	require.True(t, ok)

	table := mv.Table

	expectedColTypes := []common.ColumnType{common.TypeBigInt, common.TypeVarchar}
	expectedRf, err := common.NewRowsFactory(expectedColTypes)
	require.Nil(t, err)
	expectedRows := expectedRf.NewRows(10)
	appendRow(t, expectedRows, expectedColTypes, 1, "wincanton")
	expectedRow := expectedRows.GetRow(0)

	row, err := table.LookupInPk([]interface{}{int64(1)}, 1)
	require.Nil(t, err)
	require.NotNil(t, row)
	RowsEqual(t, &expectedRow, row, expectedColTypes)
}

func appendRow(t *testing.T, rows *common.PushRows, colTypes []common.ColumnType, colVals ...interface{}) {
	require.Equal(t, len(colVals), len(colTypes))

	for i, colType := range colTypes {
		colVal := colVals[i]
		switch colType {
		case common.TypeTinyInt, common.TypeInt, common.TypeBigInt:
			rows.AppendInt64ToColumn(i, int64(colVal.(int)))
		case common.TypeDouble:
			rows.AppendFloat64ToColumn(i, colVal.(float64))
		case common.TypeVarchar:
			rows.AppendStringToColumn(i, colVal.(string))
		}
	}
}

func RowsEqual(t *testing.T, expected *common.PushRow, actual *common.PushRow, colTypes []common.ColumnType) {
	require.Equal(t, expected.ColCount(), actual.ColCount())
	for colIndex, colType := range colTypes {
		switch colType {
		case common.TypeTinyInt, common.TypeInt, common.TypeBigInt:
			val1 := expected.GetInt64(colIndex)
			val2 := actual.GetInt64(colIndex)
			require.Equal(t, val1, val2)
		case common.TypeDecimal:
			// TODO
		case common.TypeDouble:
			val1 := expected.GetFloat64(colIndex)
			val2 := actual.GetFloat64(colIndex)
			require.Equal(t, val1, val2)
		case common.TypeVarchar:
			val1 := expected.GetString(colIndex)
			val2 := actual.GetString(colIndex)
			require.Equal(t, val1, val2)
		default:
			t.Errorf("unexpected column type %d", colType)
		}
	}
}