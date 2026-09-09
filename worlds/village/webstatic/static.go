// Package webstatic 内嵌 Agent Village 前端。
//
// 前端是单文件应用（dist/index.html，vanilla JS + SSE），直接修改该文件即可，
// 无需 npm 构建；Go embed 会把它打进二进制。
package webstatic

import "embed"

// DistFS 内嵌 webstatic/dist 下的全部前端静态文件。
//
//go:embed all:dist
var DistFS embed.FS
