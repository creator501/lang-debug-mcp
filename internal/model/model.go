// Package model 定义语言无关的统一调试模型：会话状态、停止态、快照、取值路径与预算。
//
// 上层工具只依赖本包，不直接接触 JDWP / DAP / CDP 的任何结构。语言与协议差异
// 一律由 internal/backend 下的适配层消化。
package model
