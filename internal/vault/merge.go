package vault

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/richaction/onebox-collector/internal/model"
)

type section struct {
	start, end int
	id, clock  string
}

var timeHeading = regexp.MustCompile(`^## ([01][0-9]|2[0-3]):[0-5][0-9][ \t]*$`)

func parseSections(note string) (sections []section, remaining string) {
	start := 0
	for _, match := range entryPattern.FindAllStringSubmatchIndex(note, -1) {
		sections = append(sections, manualSections(note, start, match[0])...)
		stamp, _ := strconv.ParseInt(note[match[4]:match[5]], 10, 64)
		sections = append(sections, section{match[0], match[1], note[match[2]:match[3]], time.Unix(stamp, 0).In(model.Shanghai).Format("15:04:05")})
		start = match[1]
	}
	sections = append(sections, manualSections(note, start, len(note))...)
	var residual []string
	start = 0
	for _, entry := range sections {
		if part := strings.TrimSpace(note[start:entry.start]); part != "" {
			residual = append(residual, part)
		}
		start = entry.end
	}
	if part := strings.TrimSpace(note[start:]); part != "" {
		residual = append(residual, part)
	}
	return sections, strings.Join(residual, "\n\n")
}

func manualSections(note string, start, end int) []section {
	var sections []section
	var active *section
	var fence byte
	var fenceLen int
	index := start
	for _, line := range strings.SplitAfter(note[start:end], "\n") {
		trimmed := strings.TrimSpace(line)
		if len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			c := trimmed[0]
			n := len(trimmed) - len(strings.TrimLeft(trimmed, string(c)))
			if n >= 3 && fence == 0 {
				fence, fenceLen = c, n
			} else if c == fence && n >= fenceLen && strings.TrimSpace(trimmed[n:]) == "" {
				fence = 0
			}
		}
		if fence == 0 && (strings.HasPrefix(line, "# ") || strings.HasPrefix(line, "## ")) {
			if active != nil {
				active.end = index
				sections = append(sections, *active)
				active = nil
			}
			if timeHeading.MatchString(strings.TrimSuffix(line, "\n")) {
				active = &section{start: index, clock: line[3:8] + ":00"}
			}
		}
		index += len(line)
	}
	if active != nil {
		active.end = end
		sections = append(sections, *active)
	}
	return sections
}

// Merge preserves the main note and imports identifiable additions from a copy.
func Merge(main, other []byte) ([]byte, error) {
	primary := strings.ReplaceAll(string(main), "\r\n", "\n")
	secondary := strings.ReplaceAll(string(other), "\r\n", "\n")
	if primary == secondary {
		return main, nil
	}
	current, primaryResidual := parseSections(primary)
	candidates, secondaryResidual := parseSections(secondary)
	if primaryResidual != secondaryResidual {
		return nil, ErrConflict
	}
	known := make(map[string]string, len(current))
	for _, entry := range current {
		body := strings.TrimSpace(primary[entry.start:entry.end])
		id := entry.id
		if id == "" {
			id = body
		}
		known[id] = body
	}
	var additions []section
	for _, entry := range candidates {
		body := strings.TrimSpace(secondary[entry.start:entry.end])
		id := entry.id
		if id == "" {
			id = body
		}
		if existing, ok := known[id]; ok {
			if existing != body {
				return nil, ErrConflict
			}
		} else {
			additions = append(additions, entry)
			known[id] = body
		}
	}
	if len(additions) == 0 {
		return main, nil
	}
	slices.SortStableFunc(additions, func(a, b section) int { return strings.Compare(a.clock, b.clock) })
	index := 0
	var combined strings.Builder
	for _, addition := range additions {
		offset := len(primary)
		for _, entry := range current {
			if entry.clock > addition.clock && entry.start >= index {
				offset = entry.start
				break
			}
		}
		combined.WriteString(primary[index:offset])
		if offset > index && !strings.HasSuffix(primary[index:offset], "\n\n") {
			if !strings.HasSuffix(primary[index:offset], "\n") {
				combined.WriteByte('\n')
			}
			combined.WriteByte('\n')
		}
		combined.WriteString(strings.TrimSpace(secondary[addition.start:addition.end]))
		combined.WriteString("\n\n")
		index = offset
	}
	combined.WriteString(primary[index:])
	return []byte(combined.String()), nil
}
