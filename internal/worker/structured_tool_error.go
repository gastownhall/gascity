package worker

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const (
	toolErrorUserRejection           = "user_rejection"
	toolErrorUserRejectionWithReason = "user_rejection_with_reason"
	toolErrorCommandFailure          = "command_failure"
	toolErrorFile                    = "file_error"
	toolErrorValidation              = "validation_error"
	toolErrorTimeout                 = "timeout"
	toolErrorNetwork                 = "network_error"
	toolErrorUnknown                 = "unknown"
)

type structuredToolErrorPattern struct {
	pattern  *regexp.Regexp
	category string
}

var structuredToolErrorPatterns = []structuredToolErrorPattern{
	{regexp.MustCompile(`(?i)^User denied permission\.?$`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^User did not (approve|allow|permit)`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^Permission denied by user`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^Rejected by user`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^The user doesn't want to proceed`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^The user doesn't want to take this action`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^\[Request interrupted by user`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^User canceled`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^Plan mode - tools not executed`), toolErrorUserRejection},
	{regexp.MustCompile(`(?i)^Exit code[: ]+[1-9]\d*`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)^Process exited with code [1-9]\d*`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)ELIFECYCLE.*Command failed`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)command not found`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)Shell \d+ is not running`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)No shell found`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)No task found`), toolErrorCommandFailure},
	{regexp.MustCompile(`(?i)File has been modified since read|File has been unexpectedly modified`), toolErrorFile},
	{regexp.MustCompile(`(?i)File has not been read yet`), toolErrorFile},
	{regexp.MustCompile(`(?i)File does not exist|No such file or directory|ENOENT`), toolErrorFile},
	{regexp.MustCompile(`(?i)Permission denied|EACCES`), toolErrorFile},
	{regexp.MustCompile(`(?i)No plan file found`), toolErrorFile},
	{regexp.MustCompile(`(?i)Path does not exist`), toolErrorFile},
	{regexp.MustCompile(`(?i)old_string.*not found|String to replace not found`), toolErrorValidation},
	{regexp.MustCompile(`(?i)not unique`), toolErrorValidation},
	{regexp.MustCompile(`(?i)Found \d+ matches.*replace_all is false`), toolErrorValidation},
	{regexp.MustCompile(`(?i)InputValidationError|Invalid (input|parameter|argument)`), toolErrorValidation},
	{regexp.MustCompile(`(?i)No changes to make.*old_string and new_string are exactly the same`), toolErrorValidation},
	{regexp.MustCompile(`(?i)File content.*exceeds maximum|exceeds maximum allowed`), toolErrorValidation},
	{regexp.MustCompile(`(?i)Agent type .* not found`), toolErrorValidation},
	{regexp.MustCompile(`(?i)^Request failed with status code`), toolErrorNetwork},
	{regexp.MustCompile(`ECONNREFUSED`), toolErrorNetwork},
	{regexp.MustCompile(`ENOTFOUND`), toolErrorNetwork},
	{regexp.MustCompile(`(?i)fetch failed`), toolErrorNetwork},
	{regexp.MustCompile(`(?i)Tool permission request failed`), toolErrorNetwork},
	{regexp.MustCompile(`(?i)timed? ?out|ETIMEDOUT`), toolErrorTimeout},
}

var (
	toolUseErrorTagPattern   = regexp.MustCompile(`(?i)</?tool_use_error>`)
	errorPrefixPattern       = regexp.MustCompile(`(?i)^Error:\s*`)
	rejectionReasonPattern   = regexp.MustCompile(`(?is)provided the following reason[^:]*:\s*(.+)`)
	explicitToolErrorPattern = regexp.MustCompile(`(?i)(exit code[: ]+|process exited with code )[1-9]\d*|ENOENT|EACCES|command failed|command not found|permission denied|does not exist|not found|timed? ?out|ECONNREFUSED|ENOTFOUND|fetch failed`)
	systemErrorPattern       = regexp.MustCompile(`(?i)(exit code[: ]+|process exited with code )[1-9]\d*|ENOENT|EACCES|command failed|permission denied|not found|does not exist`)
	rejectionStartPattern    = regexp.MustCompile(`^[a-zA-Z]`)
	shoutingPrefixPattern    = regexp.MustCompile(`^[A-Z_]+:`)
	multilinePattern         = regexp.MustCompile(`\n.*\n`)
)

func attachStructuredToolError(result *StructuredToolResult, block HistoryBlock, content string) *StructuredToolResult {
	if result == nil || result.Error != nil {
		return result
	}
	result.Error = structuredToolErrorForResult(result, block, content)
	return result
}

func structuredToolErrorForResult(result *StructuredToolResult, block HistoryBlock, content string) *StructuredToolError {
	message := structuredToolErrorSource(result, content, block.Text)
	if result.ExitCode != nil && *result.ExitCode != 0 && isCommandLikeErrorResult(result.Kind) {
		if message == "" {
			message = fmt.Sprintf("Exit code %d", *result.ExitCode)
		}
		return &StructuredToolError{
			Category: toolErrorCommandFailure,
			Message:  cleanToolErrorMessage(message),
		}
	}
	if !block.IsError && !looksLikeStructuredToolError(content) && !looksLikeStructuredToolError(message) {
		return nil
	}
	classified := classifyStructuredToolErrorMessage(firstNonEmptyString(content, message), block.IsError)
	if classified == nil && message != content {
		classified = classifyStructuredToolErrorMessage(message, block.IsError)
	}
	return classified
}

func classifyStructuredToolErrorMessage(content string, allowHumanRejection bool) *StructuredToolError {
	cleaned := cleanToolErrorMessage(content)
	for _, candidate := range []string{content, cleaned} {
		for _, pattern := range structuredToolErrorPatterns {
			if !pattern.pattern.MatchString(candidate) {
				continue
			}
			category := pattern.category
			userReason := ""
			if category == toolErrorUserRejection {
				userReason = extractToolErrorUserReason(content)
				if userReason != "" {
					category = toolErrorUserRejectionWithReason
				}
			}
			return &StructuredToolError{
				Category:   category,
				Message:    cleaned,
				UserReason: userReason,
			}
		}
	}
	if allowHumanRejection && looksLikeHumanToolRejection(cleaned) {
		return &StructuredToolError{
			Category:   toolErrorUserRejectionWithReason,
			Message:    cleaned,
			UserReason: cleaned,
		}
	}
	if cleaned == "" {
		return nil
	}
	return &StructuredToolError{
		Category: toolErrorUnknown,
		Message:  cleaned,
	}
}

func structuredToolErrorSource(result *StructuredToolResult, content string, blockText string) string {
	for _, candidate := range []string{
		result.Stderr,
		result.Output,
		result.Content,
		result.Text,
		content,
		blockText,
	} {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || looksLikeStructuredJSON(candidate) {
			continue
		}
		return candidate
	}
	return ""
}

func looksLikeStructuredJSON(text string) bool {
	if text == "" {
		return false
	}
	if !strings.HasPrefix(text, "{") && !strings.HasPrefix(text, "[") {
		return false
	}
	return json.Valid([]byte(text))
}

func cleanToolErrorMessage(content string) string {
	cleaned := errorPrefixPattern.ReplaceAllString(content, "")
	cleaned = toolUseErrorTagPattern.ReplaceAllString(cleaned, "")
	return strings.TrimSpace(cleaned)
}

func extractToolErrorUserReason(content string) string {
	match := rejectionReasonPattern.FindStringSubmatch(content)
	if len(match) < 2 {
		return ""
	}
	return cleanToolErrorMessage(match[1])
}

func looksLikeStructuredToolError(content string) bool {
	cleaned := cleanToolErrorMessage(content)
	if cleaned == "" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(content), "Error:") ||
		strings.Contains(strings.ToLower(content), "<tool_use_error>") ||
		explicitToolErrorPattern.MatchString(cleaned)
}

func looksLikeHumanToolRejection(cleaned string) bool {
	if len(cleaned) > 200 {
		return false
	}
	if systemErrorPattern.MatchString(cleaned) {
		return false
	}
	return rejectionStartPattern.MatchString(cleaned) &&
		strings.Contains(cleaned, " ") &&
		!shoutingPrefixPattern.MatchString(cleaned) &&
		!multilinePattern.MatchString(cleaned)
}

func isCommandLikeErrorResult(kind string) bool {
	switch kind {
	case "bash", "python", "task":
		return true
	default:
		return false
	}
}
