// Package plugins 汇总所有编译进二进制的 connector 插件。
//
// 这是插件清单的唯一入口：新增上游类型时在此加一行 blank import；
// 删减时删掉对应行即可——插件不会被编译进二进制，
// 前端类型下拉、后端能力检查都会随之消失，无需改其它代码。
package plugins

import (
	_ "github.com/bejix/upstream-ops/backend/connector/aiapi"
	_ "github.com/bejix/upstream-ops/backend/connector/newapi"
	_ "github.com/bejix/upstream-ops/backend/connector/sub2api"
)
