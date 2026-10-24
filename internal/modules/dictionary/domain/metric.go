// Package domain 定义数据分析拥有的指标字典值。
package domain

import "time"

// Metric 描述一项已发布分析指标定义。
type Metric struct {
	ID                 string `json:"id" gorm:"primaryKey"`
	Version            uint64 `json:"version"`
	Origin             string `json:"origin"`
	Enabled            bool   `json:"enabled"`
	CalculationBinding string `json:"calculation_binding" gorm:"-"`
	CanEnable          bool   `json:"can_enable" gorm:"-"`
	Code               string `json:"code" gorm:"index:tenant_code,unique"`
	Name               string `json:"name"`
	Dashboard          string `json:"dashboard"`
	Definition         string `json:"definition"`
	Formula            string `json:"formula"`
	Source             string `json:"source"`
	Period             string `json:"period"`
	Status             string `json:"status"`
}

type MetricVersion struct {
	Metric    Metric    `json:"metric"`
	Operation string    `json:"operation"`
	ActorID   string    `json:"actor_id"`
	CreatedAt time.Time `json:"created_at"`
}

// Catalog 是已发布只读指标字典。
type Catalog struct {
	Version string   `json:"version"`
	Source  string   `json:"source"`
	Status  string   `json:"status"`
	Note    string   `json:"note"`
	Metrics []Metric `json:"metrics"`
}
