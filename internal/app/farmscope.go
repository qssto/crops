package app

import (
	"agri-steward/internal/agent/graph"
	"agri-steward/internal/repository"
)

// withFarmScope 装配 P1 L1-B 的按农场隔离依赖：知识区域取本农场 farm_profile.region。
func (a *App) withFarmScope(cfg *graph.Config) {
	cfg.Regions = repository.FarmRegions{DB: a.DB}
}
