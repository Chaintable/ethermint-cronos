package types

type TraceResult struct {
	Traces           []Trace  `json:"traces"`
	Events           []Event  `json:"events"`
	ErrorEvents      []Event  `json:"error_events"`
	ErrorTraces      []Trace  `json:"error_traces"`
	StorageContracts []string `json:"storage_contracts"`
}
