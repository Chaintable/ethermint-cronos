package types

type TraceResult struct {
	Transaction      Transaction          `json:"transaction"`
	StateDiff        TransactionStateDiff `json:"statediff"`
	Traces           []Trace              `json:"traces"`
	Events           []Event              `json:"events"`
	ErrorEvents      []Event              `json:"error_events"`
	ErrorTraces      []Trace              `json:"error_traces"`
	StorageContracts []string             `json:"storage_contracts"`
}
