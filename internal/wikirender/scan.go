package wikirender

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/carsteneu/yesmem/internal/codescan"
)

type scanFile struct {
	Path       string   `json:"Path"`
	Language   string   `json:"Language"`
	LOC        int      `json:"LOC"`
	IsTest     bool     `json:"IsTest"`
	TestCount  int      `json:"TestCount"`
	Signatures []string `json:"Signatures"`
	Imports    []string `json:"Imports"`
}

type scanCoupling struct {
	FileA string `json:"FileA"`
	FileB string `json:"FileB"`
}

type scanData struct {
	Files          []scanFile     `json:"Files"`
	ChangeCoupling []scanCoupling `json:"ChangeCoupling"`
}

func (s *renderState) loadScan() error {
	if len(s.cfg.ScanJSON) == 0 {
		// project_scan rows are keyed by the checkout root (codescan
		// .ScanCacheKey), never by a display name. A named project is resolved
		// to its directory first, because the name alone names no path to look
		// up; a project without a directory has no scan row (YM-277).
		dir := s.projectPath
		if dir == "" {
			return nil
		}
		var raw []byte
		err := s.cfg.Store.DB().QueryRow(
			`SELECT scan_json FROM project_scan WHERE project = ?`,
			codescan.ScanCacheKey(dir),
		).Scan(&raw)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("loadScan from DB: %w", err)
		}
		s.cfg.ScanJSON = raw
	}
	var sd scanData
	if err := json.Unmarshal(s.cfg.ScanJSON, &sd); err != nil {
		return fmt.Errorf("loadScan unmarshal: %w", err)
	}

	// Build code graph from cached scan if no live graph was passed.
	if s.cfg.CodeGraph == nil {
		var sr codescan.ScanResult
		if err := json.Unmarshal(s.cfg.ScanJSON, &sr); err == nil {
			s.cfg.CodeGraph = codescan.BuildCodeGraph(&sr)
		}
	}
	for i := range sd.Files {
		f := &sd.Files[i]
		s.scanFiles[f.Path] = f
	}
	couplingRaw := map[string]map[string]int{}
	for _, cc := range sd.ChangeCoupling {
		if couplingRaw[cc.FileA] == nil {
			couplingRaw[cc.FileA] = map[string]int{}
		}
		couplingRaw[cc.FileA][cc.FileB]++
		if couplingRaw[cc.FileB] == nil {
			couplingRaw[cc.FileB] = map[string]int{}
		}
		couplingRaw[cc.FileB][cc.FileA]++
	}
	for a, deps := range couplingRaw {
		var items []CoEdit
		for b, n := range deps {
			items = append(items, CoEdit{Path: b, Count: n})
		}
		sort.Slice(items, func(i, j int) bool { return items[i].Count > items[j].Count })
		if len(items) > 5 {
			items = items[:5]
		}
		s.coupling[a] = items
	}
	return nil
}

func (s *renderState) lookupScan(path string) *scanFile {
	if f, ok := s.scanFiles[path]; ok {
		return f
	}
	for k, f := range s.scanFiles {
		if strings.HasSuffix(path, k) {
			return f
		}
	}
	return nil
}
