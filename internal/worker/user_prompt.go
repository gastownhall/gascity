package worker

import (
	"regexp"
	"strings"
)

var (
	historyIDEOpenedFileTagPattern = regexp.MustCompile(`(?is)<ide_opened_file>(.*?)</ide_opened_file>`)
	historyIDESelectionTagPattern  = regexp.MustCompile(`(?is)<ide_selection>(.*?)</ide_selection>`)
	historyOpenedFilePathPattern   = regexp.MustCompile(`(?i)(user opened the file|opened the file)\s+(.+?)\s+in the IDE`)
	historyUploadedFileLinePattern = regexp.MustCompile(`^- (.+?) \(([^,]+), ([^)]+)\): (.+)$`)
)

func parseHistoryUserPrompt(text string) *HistoryUserPrompt {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	textWithoutUploads, uploads := parseHistoryUploadedFiles(text)
	prompt := &HistoryUserPrompt{
		Text:          stripHistoryIDEMetadata(textWithoutUploads),
		OpenedFiles:   parseHistoryOpenedFiles(textWithoutUploads),
		Selections:    parseHistorySelections(textWithoutUploads),
		UploadedFiles: uploads,
	}
	if prompt.Text == "" && len(prompt.OpenedFiles) == 0 && len(prompt.Selections) == 0 && len(prompt.UploadedFiles) == 0 {
		return nil
	}
	return prompt
}

func parseHistoryUploadedFiles(text string) (string, []HistoryUploadedFile) {
	const uploadMarker = "\n\nUser uploaded files:\n"
	markerIndex := strings.Index(text, uploadMarker)
	if markerIndex < 0 {
		return text, nil
	}
	uploadSection := text[markerIndex+len(uploadMarker):]
	uploads := make([]HistoryUploadedFile, 0)
	for _, line := range strings.Split(uploadSection, "\n") {
		match := historyUploadedFileLinePattern.FindStringSubmatch(strings.TrimSpace(line))
		if len(match) != 5 {
			continue
		}
		uploads = append(uploads, HistoryUploadedFile{
			OriginalName: strings.TrimSpace(match[1]),
			Size:         strings.TrimSpace(match[2]),
			MIMEType:     strings.TrimSpace(match[3]),
			FilePath:     strings.TrimSpace(match[4]),
		})
	}
	return text[:markerIndex], uploads
}

func parseHistoryOpenedFiles(text string) []string {
	matches := historyIDEOpenedFileTagPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	files := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		pathMatch := historyOpenedFilePathPattern.FindStringSubmatch(strings.TrimSpace(match[1]))
		if len(pathMatch) == 3 {
			files = append(files, strings.TrimSpace(pathMatch[2]))
		}
	}
	return files
}

func parseHistorySelections(text string) []HistoryUserSelection {
	matches := historyIDESelectionTagPattern.FindAllStringSubmatch(text, -1)
	if len(matches) == 0 {
		return nil
	}
	selections := make([]HistoryUserSelection, 0, len(matches))
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		if selection := strings.TrimSpace(match[1]); selection != "" {
			selections = append(selections, HistoryUserSelection{Text: selection})
		}
	}
	return selections
}

func stripHistoryIDEMetadata(text string) string {
	text = historyIDEOpenedFileTagPattern.ReplaceAllString(text, "")
	text = historyIDESelectionTagPattern.ReplaceAllString(text, "")
	return strings.TrimSpace(text)
}
