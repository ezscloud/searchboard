// Package main searchboard：Elasticsearch/OpenSearch 监控管理台。
//
// embed 前端基座（searchboard/web 子树）；外部 public_dir 作为覆盖层
// 叠加其上（logo.png 等品牌资源优先取覆盖层）。
package main

import "embed"

//go:embed web/*
var webFS embed.FS
