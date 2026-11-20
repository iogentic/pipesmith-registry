package server

import (
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/iogentic/pipesmith-registry/internal/catalog"
	"github.com/iogentic/pipesmith-registry/internal/metrics"
	"github.com/iogentic/pipesmith-registry/internal/rest"
)

// The figures of task 7.23 of the phase 7 plan: the counters since the start
// with those of the log targets, the requests and downloads per day on the
// paths of the contract (S43 of docs/ui/API.md), and the series of
// metrics.sqlite for the cards of Overview. Each is read with the permission
// of the health checks or the one the contract names for S43.

const metricsRead = "nexus:metrics:read"

// maxDays is the longest span of the figures per day.
const maxDays = 366

var figureRoutes = []route{
	get("/api/metrics", perm(atlasRead, metricsRead), (*Server).metricsTotals),
	get("/api/metrics/requests", perm(atlasRead, metricsRead), (*Server).metricsRequests),
	get("/api/metrics/downloads", perm(atlasRead, metricsRead), (*Server).metricsDownloads),
	get("/api/figures", perm(atlasRead, metricsRead), (*Server).figureSeries),
}

type totalsXO struct {
	metrics.Totals
	LogTargets []metrics.TargetFigures `json:"logTargets"`
}

func (s *Server) metricsTotals(w *recorder, req *request, _ []string) error {
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: totalsXO{Totals: s.figures.Totals(), LogTargets: s.figures.Targets()}})
}

// errDays is the refusal of a span the figures per day do not take.
var errDays = errors.New("from and to are days of the form YYYY-MM-DD, from not after to, at most 366 days")

// daySpan reads from and to of the query; without both it is the 30 days up
// to today.
func (s *Server) daySpan(req *request) (from, to time.Time, err error) {
	q, err := url.ParseQuery(req.query)
	if err != nil {
		return from, to, errDays
	}
	today := s.figures.Now().UTC().Truncate(24 * time.Hour)
	to, from = today, today.AddDate(0, 0, -29)
	if v := q.Get("to"); v != "" {
		if to, err = time.Parse(catalog.DayLayout, v); err != nil {
			return from, to, errDays
		}
		from = to.AddDate(0, 0, -29)
	}
	if v := q.Get("from"); v != "" {
		if from, err = time.Parse(catalog.DayLayout, v); err != nil {
			return from, to, errDays
		}
	}
	if from.After(to) || to.Sub(from) >= maxDays*24*time.Hour {
		return from, to, errDays
	}
	return from, to, nil
}

type dayXO struct {
	Day      string `json:"day"`
	Requests int64  `json:"requests"`
	BytesOut int64  `json:"bytesOut"`
}

func (s *Server) metricsRequests(w *recorder, req *request, _ []string) error {
	from, to, err := s.daySpan(req)
	if err != nil {
		s.message(w, req, http.StatusBadRequest, err.Error())
		return nil
	}
	rows, err := s.figures.RequestDays(from.Format(catalog.DayLayout), to.Format(catalog.DayLayout))
	if err != nil {
		return err
	}
	byDay := map[string]catalog.RequestDay{}
	for _, r := range rows {
		byDay[r.Day] = r
	}
	days := []dayXO{}
	for d := from; !d.After(to); d = d.AddDate(0, 0, 1) {
		day := d.Format(catalog.DayLayout)
		days = append(days, dayXO{Day: day, Requests: byDay[day].Requests, BytesOut: byDay[day].BytesSent})
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: map[string]any{
		"from": from.Format(catalog.DayLayout), "to": to.Format(catalog.DayLayout), "days": days}})
}

type downloadXO struct {
	Repository string `json:"repository"`
	Day        string `json:"day"`
	Count      int64  `json:"count"`
	Bytes      int64  `json:"bytes"`
}

func (s *Server) metricsDownloads(w *recorder, req *request, _ []string) error {
	from, to, err := s.daySpan(req)
	if err != nil {
		s.message(w, req, http.StatusBadRequest, err.Error())
		return nil
	}
	rows, err := s.figures.Downloads(from.Format(catalog.DayLayout), to.Format(catalog.DayLayout))
	if err != nil {
		return err
	}
	out := []downloadXO{}
	for _, r := range rows {
		out = append(out, downloadXO{Repository: r.Repo, Day: r.Day, Count: r.Count, Bytes: r.Bytes})
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: map[string]any{
		"from": from.Format(catalog.DayLayout), "to": to.Format(catalog.DayLayout), "rows": out}})
}

func (s *Server) figureSeries(w *recorder, req *request, _ []string) error {
	q, err := url.ParseQuery(req.query)
	name := q.Get("range")
	if name == "" {
		name = "24h"
	}
	series, serr := s.figures.Series(name)
	if err != nil || errors.Is(serr, metrics.ErrRange) {
		s.message(w, req, http.StatusBadRequest, "range is one of 24h, 7d and 30d")
		return nil
	}
	if serr != nil {
		return serr
	}
	return s.writeAnswer(w, req, rest.Answer{Status: http.StatusOK, Body: series})
}
