//
// This source code is a modified form of original source from the TiDB project, which has the following copyright header(s):
//

// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package expression

import (
	"github.com/squareup/pranadb/tidb"
	"math"
	"strings"
	"time"

	"github.com/pingcap/parser/ast"
	"github.com/pingcap/parser/mysql"
	"github.com/pingcap/parser/terror"
	"github.com/squareup/pranadb/tidb/sessionctx"
	"github.com/squareup/pranadb/tidb/types"
	driver "github.com/squareup/pranadb/tidb/types/parser_driver"
)

func boolToInt64(v bool) int64 {
	if v {
		return 1
	}
	return 0
}

// GetTimeValue gets the time value with type tp.
func GetTimeValue(ctx sessionctx.Context, v interface{}, tp byte, fsp int8) (d types.Datum, err error) {
	value := types.NewTime(types.ZeroCoreTime, tp, fsp)

	sc := ctx.GetSessionVars().StmtCtx
	switch x := v.(type) {
	case string:
		upperX := strings.ToUpper(x)
		if upperX == strings.ToUpper(ast.CurrentTimestamp) {
			defaultTime, err := getStmtTimestamp(ctx)
			if err != nil {
				return d, err
			}
			value.SetCoreTime(types.FromGoTime(defaultTime.Truncate(time.Duration(math.Pow10(9-int(fsp))) * time.Nanosecond)))
			if tp == mysql.TypeTimestamp || tp == mysql.TypeDatetime {
				err = value.ConvertTimeZone(time.Local, ctx.GetSessionVars().Location())
				if err != nil {
					return d, err
				}
			}
		} else if upperX == types.ZeroDatetimeStr {
			value, err = types.ParseTimeFromNum(sc, 0, tp, fsp)
			terror.Log(err)
		} else {
			value, err = types.ParseTime(sc, x, tp, fsp)
			if err != nil {
				return d, err
			}
		}
	case *driver.ValueExpr:
		switch x.Kind() {
		case types.KindString:
			value, err = types.ParseTime(sc, x.GetString(), tp, fsp)
			if err != nil {
				return d, err
			}
		case types.KindInt64:
			value, err = types.ParseTimeFromNum(sc, x.GetInt64(), tp, fsp)
			if err != nil {
				return d, err
			}
		case types.KindNull:
			return d, nil
		default:
			return d, tidb.ErrDefaultValue
		}
	case *ast.FuncCallExpr:
		if x.FnName.L == ast.CurrentTimestamp {
			d.SetString(strings.ToUpper(ast.CurrentTimestamp), mysql.DefaultCollationName)
			return d, nil
		}
		return d, tidb.ErrDefaultValue
	case *ast.UnaryOperationExpr:
		// support some expression, like `-1`
		v, err := EvalAstExpr(ctx, x)
		if err != nil {
			return d, err
		}
		ft := types.NewFieldType(mysql.TypeLonglong)
		xval, err := v.ConvertTo(ctx.GetSessionVars().StmtCtx, ft)
		if err != nil {
			return d, err
		}

		value, err = types.ParseTimeFromNum(sc, xval.GetInt64(), tp, fsp)
		if err != nil {
			return d, err
		}
	default:
		return d, nil
	}
	d.SetMysqlTime(value)
	return d, nil
}

// if timestamp session variable set, use session variable as current time, otherwise use cached time
// during one sql statement, the "current_time" should be the same
func getStmtTimestamp(ctx sessionctx.Context) (time.Time, error) {
	now := time.Now()
	if ctx == nil {
		return now, nil
	}
	return ctx.GetSessionVars().StmtCtx.GetNow(), nil
}
