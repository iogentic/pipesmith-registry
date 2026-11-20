package metrics

// TargetFigures are the counters of one log target (docs/LOGGING.md,
// section Counters) and the lines it holds now.
type TargetFigures struct {
	Name string `json:"name"`
	Kind string `json:"kind"`
	// QueueDepth are the lines queued and not settled yet: queued minus
	// sent, rejected and failed, the batch in flight included.
	QueueDepth int64 `json:"queueDepth"`
	Queued     int64 `json:"queued"`
	Sent       int64 `json:"sent"`
	Dropped    int64 `json:"dropped"`
	Failed     int64 `json:"failed"`
	Rejected   int64 `json:"rejected"`
	Cut        int64 `json:"cut"`
	Attempts   int64 `json:"attempts"`
	Errors     int64 `json:"errors"`
}

// Targets are the figures of the running log targets, in the order of the
// setting; none without targets.
func (r *Recorder) Targets() []TargetFigures {
	out := []TargetFigures{}
	if r == nil || r.targets == nil {
		return out
	}
	for _, s := range r.targets() {
		out = append(out, TargetFigures{Name: s.Target, Kind: s.Kind, QueueDepth: max(s.Queued-s.Sent-s.Rejected-s.Failed, 0),
			Queued: s.Queued, Sent: s.Sent, Dropped: s.Dropped, Failed: s.Failed, Rejected: s.Rejected, Cut: s.Cut,
			Attempts: s.Attempts, Errors: s.Errors})
	}
	return out
}
