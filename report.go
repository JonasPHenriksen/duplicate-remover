package main

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// report writes the reports and prints the summary.
func (p *plan) report(prog string, out, errOut io.Writer) int {
	summary, err := p.writeReports(prog)
	if err != nil {
		fmt.Fprintf(errOut, "  ! could not write the reports: %v\n", err)
		return 1
	}
	fmt.Fprintf(out, "\n%s\n", summary)
	return 0
}

// writeReports writes duplicates.csv, not-media.csv and summary.txt into the
// quarantine folder and returns the summary.
func (p *plan) writeReports(prog string) (string, error) {
	var dups strings.Builder
	dups.WriteString("group,role,path,size,reason\n")
	for i, g := range p.groups {
		dups.WriteString(csvRow(i+1, "KEPT", filepath.FromSlash(g.keeper.rel), g.keeper.size,
			whyKept(g.keeper, g.losers[0])))
		for _, f := range g.losers {
			dups.WriteString(csvRow(i+1, "quarantined", filepath.FromSlash(f.rel), f.size,
				"byte-for-byte identical"))
		}
	}

	var other strings.Builder
	other.WriteString("path,size,extension\n")
	for _, f := range p.scan.other {
		other.WriteString(csvRow(filepath.FromSlash(f.rel), f.size, or(extension(filepath.Base(f.path)), "(none)")))
	}

	var dupFiles int
	var dupBytes, otherBytes int64
	for _, g := range p.groups {
		for _, f := range g.losers {
			dupFiles++
			dupBytes += f.size
		}
	}
	var exts []string // most common first, ties in the order first seen
	counts := map[string]int{}
	for _, f := range p.scan.other {
		otherBytes += f.size
		ext := or(extension(filepath.Base(f.path)), "(no extension)")
		if counts[ext] == 0 {
			exts = append(exts, ext)
		}
		counts[ext]++
	}
	sort.SliceStable(exts, func(i, j int) bool { return counts[exts[i]] > counts[exts[j]] })

	mode := "preview only, nothing moved"
	if p.applied {
		mode = "APPLIED"
	}
	lines := []string{
		"photo_cleanup summary",
		strings.Repeat("=", 60),
		"folder            : " + p.root,
		"mode              : " + mode,
		"",
		fmt.Sprintf("duplicate groups  : %d", len(p.groups)),
		fmt.Sprintf("duplicate files   : %d  (%s)", dupFiles, human(dupBytes)),
		fmt.Sprintf("non-media files   : %d  (%s)", len(p.scan.other), human(otherBytes)),
		fmt.Sprintf("total to move     : %d  (%s)", len(p.moves), human(dupBytes+otherBytes)),
		fmt.Sprintf("skipped (links)   : %d", p.scan.skipped),
		fmt.Sprintf("skipped (system)  : %d", p.scan.system),
	}
	if p.failed > 0 {
		lines = append(lines, fmt.Sprintf("failed to move    : %d", p.failed))
	}
	lines = append(lines, "", "non-media by type:")
	for _, ext := range exts {
		lines = append(lines, fmt.Sprintf("   %-14s %d", ext, counts[ext]))
	}
	lines = append(lines,
		"",
		"Detail: duplicates.csv, not-media.csv",
		"",
		"Everything moved keeps its original relative path under _Quarantine/,",
		"so you can drag a whole subfolder back, or restore everything with:",
		"   "+shellQuote(prog)+" "+shellQuote(p.root)+" --undo",
	)
	summary := strings.Join(lines, "\n")

	dir := filepath.Join(p.root, quarantine)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	for name, content := range map[string]string{
		"duplicates.csv": dups.String(),
		"not-media.csv":  other.String(),
		"summary.txt":    summary + "\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			return "", err
		}
	}
	return summary, nil
}

// csvRow formats one CSV line, quoting the cells that need it.
func csvRow(values ...any) string {
	cells := make([]string, len(values))
	for i, v := range values {
		s := fmt.Sprint(v)
		if strings.ContainsAny(s, ",\"\n\r") {
			s = `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		cells[i] = s
	}
	return strings.Join(cells, ",") + "\n"
}

// shellQuote quotes s so it can be pasted into a terminal as one argument.
func shellQuote(s string) string {
	if runtime.GOOS == "windows" {
		if strings.ContainsAny(s, " &()[]{}^=;!'+,`~%") {
			return `"` + s + `"`
		}
		return s
	}
	safe := s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !('a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' ||
			strings.ContainsRune("@%+=:,./-_", r))
	}) < 0
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// human formats a byte count: 512 B, 1.5 KB, 3.2 GB.
func human(n int64) string {
	if -1024 < n && n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := []string{"KB", "MB", "GB", "TB"}
	f, i := float64(n)/1024, 0
	for math.Abs(f) >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func or(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
