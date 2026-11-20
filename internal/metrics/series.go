package metrics

import (
	"errors"
	"time"
)

// Ranges are the spans of a series and the width of one point: 288 points
// of 5 minutes for 24 hours, 336 of 30 minutes for 7 days, 360 of 2 hours
// for 30 days.
var Ranges = map[string]struct{ Span, Step time.Duration }{
	"24h": {24 * time.Hour, 5 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 30 * time.Minute},
	"30d": {30 * 24 * time.Hour, 2 * time.Hour},
}

// ErrRange is a range that is not a key of Ranges.
var ErrRange = errors.New("metrics: the range is one of 24h, 7d and 30d")

// Series is the answer of the figures of the cards.
type Series struct {
	Enabled     bool       `json:"enabled"`
	Range       string     `json:"range"`
	StepSeconds int        `json:"stepSeconds"`
	From        string     `json:"from"`
	To          string     `json:"to"`
	Points      []SeriesXO `json:"points"`
}

// SeriesXO is one point of a Series.
type SeriesXO struct {
	Time          string  `json:"time"`
	Requests      int64   `json:"requests"`
	BytesSent     int64   `json:"bytesSent"`
	BytesReceived int64   `json:"bytesReceived"`
	CPUSeconds    float64 `json:"cpuSeconds"`
	MemoryBytes   int64   `json:"memoryBytes"`
}

// AttachSamples gives the recorder the samples it answers Series from.
func (r *Recorder) AttachSamples(store *Store) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples = store
}

// Series are the points of a range up to now; without samples the answer is
// not enabled and has no points.
func (r *Recorder) Series(name string) (Series, error) {
	rng, ok := Ranges[name]
	if !ok {
		return Series{}, ErrRange
	}
	out := Series{Range: name, StepSeconds: int(rng.Step / time.Second), Points: []SeriesXO{}}
	to := r.Now().UTC().Truncate(rng.Step).Add(rng.Step)
	from := to.Add(-rng.Span)
	out.From, out.To = from.Format(time.RFC3339), to.Format(time.RFC3339)
	if r == nil {
		return out, nil
	}
	r.mu.Lock()
	store := r.samples
	r.mu.Unlock()
	if store == nil {
		return out, nil
	}
	points, err := store.Series(from, to, rng.Step)
	if err != nil {
		return out, err
	}
	out.Enabled = true
	for _, p := range points {
		out.Points = append(out.Points, SeriesXO{Time: p.At.Format(time.RFC3339), Requests: p.Requests, BytesSent: p.BytesSent,
			BytesReceived: p.BytesReceived, CPUSeconds: p.CPU.Seconds(), MemoryBytes: p.MemoryBytes})
	}
	return out, nil
}
