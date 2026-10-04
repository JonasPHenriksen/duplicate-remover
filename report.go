package main

import (
	"fmt"
	"math"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// report writes the reports and prints the summary.
func (p *plan) report(prog string, r *reporter) int {
	summary, err := p.writeReports(prog)
	if err != nil {
		r.warnf("  ! could not write the reports: %v\n", err)
		return 1
	}
	r.printf("\n%s\n", summary)
	return 0
}

// typeOf is a non-media file's extension, or "(none)", as not-media.csv lists it.
func typeOf(f *file) string {
	return or(extension(path.Base(f.rel)), "(none)")
}

// groupOf is how the graphical interface sorts non-media files: by type, with
// the empty ones apart, since an empty "photo.jpg" is no photo either.
func groupOf(f *file) string {
	if f.size == 0 {
		return "(empty)"
	}
	return typeOf(f)
}

// shown is how the reports and the page show a file: by its path inside the
// folder, or in full when it is in the reference folder.
func shown(f *file) string {
	if f.ref {
		return f.path
	}
	return filepath.FromSlash(f.rel)
}

// writeReports writes duplicates.csv, not-media.csv and summary.txt into the
// quarantine folder, and similar.csv when it looked for look-alikes, and
// returns the summary.
func (p *plan) writeReports(prog string) (string, error) {
	var dups strings.Builder
	dups.WriteString("group,role,path,size,reason\n")
	for i, g := range p.groups {
		dups.WriteString(csvRow(i+1, "KEPT", shown(g.keeper), g.keeper.size, reasons[p.ru.why(g)]))
		same := "byte-for-byte identical"
		if g.kind == "tags" {
			same = "same picture, different tags"
		}
		for _, f := range g.losers {
			dups.WriteString(csvRow(i+1, "quarantined", shown(f), f.size, same))
		}
	}

	// Look-alikes move only when chosen: the ones that did are those planned
	// to go to _Quarantine/similar in a plan that was applied.
	alikeMoved := map[string]bool{}
	if p.applied {
		for _, m := range p.moves {
			if strings.HasPrefix(m.to, quarantine+"/"+similarDir+"/") {
				alikeMoved[m.from] = true
			}
		}
	}
	var alike strings.Builder
	alike.WriteString("group,role,path,size,width,height\n")
	for i, g := range p.similar {
		alike.WriteString(csvRow(i+1, "suggested keep", shown(g.keeper), g.keeper.size, g.keeper.w, g.keeper.h))
		for _, f := range g.losers {
			role := "look-alike"
			if alikeMoved[f.rel] {
				role = "quarantined"
			}
			alike.WriteString(csvRow(i+1, role, shown(f), f.size, f.w, f.h))
		}
	}

	var other strings.Builder
	other.WriteString("path,size,extension\n")
	for _, f := range p.scan.other {
		other.WriteString(csvRow(filepath.FromSlash(f.rel), f.size, typeOf(f)))
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
		ext := or(extension(path.Base(f.rel)), "(no extension)")
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
	var alikeFiles int
	var alikeBytes int64
	for _, m := range p.moves {
		if alikeMoved[m.from] {
			alikeFiles++
			alikeBytes += m.size
		}
	}
	nonMedia := fmt.Sprintf("non-media files   : %d  (%s)", len(p.scan.other), human(otherBytes))
	if p.ru.DuplicatesOnly {
		nonMedia = "non-media files   : left alone, duplicates only"
	}
	lines := []string{
		"photo_cleanup summary",
		strings.Repeat("=", 60),
		"folder            : " + p.root,
		"mode              : " + mode,
	}
	if options := p.ru.summary(); options != "" {
		lines = append(lines, "options           : "+options)
	}
	lines = append(lines,
		"",
		fmt.Sprintf("duplicate groups  : %d", len(p.groups)),
		fmt.Sprintf("duplicate files   : %d  (%s)", dupFiles, human(dupBytes)),
		nonMedia,
	)
	if p.ru.Similar != "" && alikeFiles > 0 {
		lines = append(lines, fmt.Sprintf("look-alikes moved : %d  (%s), see similar.csv", alikeFiles, human(alikeBytes)))
	} else if p.ru.Similar != "" {
		lines = append(lines, fmt.Sprintf("look-alike groups : %d, see similar.csv; none move unless chosen", len(p.similar)))
	}
	var moveBytes int64
	for _, m := range p.moves {
		moveBytes += m.size
	}
	lines = append(lines,
		fmt.Sprintf("total to move     : %d  (%s)", len(p.moves), human(moveBytes)),
		fmt.Sprintf("skipped (links)   : %d", p.scan.skipped),
		fmt.Sprintf("skipped (system)  : %d", p.scan.system),
	)
	if p.scan.leftOut > 0 {
		lines = append(lines, fmt.Sprintf("folders left out  : %d", p.scan.leftOut))
	}
	if p.failed > 0 {
		lines = append(lines, fmt.Sprintf("failed to move    : %d", p.failed))
	}
	if p.removed > 0 {
		lines = append(lines, fmt.Sprintf("emptied, removed  : %d folders", p.removed))
	}
	lines = append(lines, "", "non-media by type:")
	for _, ext := range exts {
		lines = append(lines, fmt.Sprintf("   %-14s %d", ext, counts[ext]))
	}
	detail := "Detail: duplicates.csv, not-media.csv"
	if p.ru.Similar != "" {
		detail += ", similar.csv"
	}
	lines = append(lines,
		"",
		detail,
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
	reports := map[string]string{
		"duplicates.csv": dups.String(),
		"not-media.csv":  other.String(),
		"summary.txt":    summary + "\n",
	}
	if p.ru.Similar != "" {
		reports["similar.csv"] = alike.String()
	}
	for name, content := range reports {
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
