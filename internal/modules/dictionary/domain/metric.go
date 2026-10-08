// Package domain defines metric-dictionary values owned by data analysis.
package domain

import "time"

// Metric documents one published analytical metric definition.
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

// Catalog is the published read-only metric dictionary.
type Catalog struct {
	Version string   `json:"version"`
	Source  string   `json:"source"`
	Status  string   `json:"status"`
	Note    string   `json:"note"`
	Metrics []Metric `json:"metrics"`
}
