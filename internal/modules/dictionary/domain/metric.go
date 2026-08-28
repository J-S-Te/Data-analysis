// Package domain defines metric-dictionary values owned by data analysis.
package domain

// Metric documents one published analytical metric definition.
type Metric struct {
	Code       string `json:"code"`
	Name       string `json:"name"`
	Dashboard  string `json:"dashboard"`
	Definition string `json:"definition"`
	Formula    string `json:"formula"`
	Source     string `json:"source"`
	Period     string `json:"period"`
	Status     string `json:"status"`
}

// Catalog is the published read-only metric dictionary.
type Catalog struct {
	Version string   `json:"version"`
	Source  string   `json:"source"`
	Status  string   `json:"status"`
	Note    string   `json:"note"`
	Metrics []Metric `json:"metrics"`
}
