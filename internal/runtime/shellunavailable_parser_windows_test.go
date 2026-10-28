//go:build windows

package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestPowerShellCommandScannerMatchesParser(t *testing.T) {
	inputs := []string{
		"",
		"   ",
		"# comment only\n",
		"# ordinary order comment\nsh -c harmless",
		"<# block comment #>\nsh -c harmless",
		"bash -c x",
		"ba`sh -c x",
		"s`h -c x",
		". bash -c x",
		`.\x.sh`,
		`..\x.sh`,
		"& bash -c x",
		`&'C:\tools\x.sh' arg`,
		`& 'C:\tools\O''Brien\sh.exe' -c x`,
		`& "ba` + "`" + `sh" -c x`,
		"& ('sh') -c x",
		"& (\"sh\") -c x",
		"& ('s' + 'h') -c x",
		`"bash"`,
		`'report.sh'`,
		"gc beads health --quiet",
		"gc dolt health --json | gc dolt health-check",
		"cd .; bash -c x",
		"$env:X='1'; bash -c x",
		"Write-Output before; bash -c x",
		"a | bash",
		"a && bash",
		"a || bash",
		"Start-Process bash",
		"Start-Process -FilePath bash -ArgumentList x",
		"Invoke-Expression 'bash -c x'",
		"$p='bash'; & $p -c x",
		"$x = bash -c x",
		"$x += bash -c x",
		"$null = sh -c x",
		"return bash -c x",
		"throw bash",
		"(bash -c x)",
		"Write-Output (bash -c x)",
		"echo $(sh -c x)",
		"\"$(bash -c x)\"",
		"test -n \"$(bash -c 'echo x')\"",
		"bd list --json > \"$(sh -c mktemp)\"",
		"1 | ForEach-Object { bash -c x }",
		"1 | % { sh -c x }",
		"if (bash -c x) { 'y' }",
		"while (sh -c x) { break }",
		"@(bash -c x)",
		"@(& 'C:\\tools\\static.sh')",
		"[void](bash -c x)",
		".{bash -c x}",
		".('bash') -c x",
		"param()\nend { bash -c x }",
		"begin { sh -c x }",
		"process { bash -c x }",
		"& \u2018bash\u2019 -c x",
		"& \u201cbash\u201d -c x",
		"function f { bash -c x }; f",
		"switch ($x) { sh { 'a' } default { 'b' } }",
		"Write-Output `\n bash",
		"@{ sh = 1 }",
		"Write-Output @'\nplain text\nbash\n'@",
		"if ($true) { bash -c x }",
		"if ($true) {\n# c\nsh -c x\n}",
		"if ($true) { & ('sh') -c x }",
		"foreach ($x in 1) { bash -c x }",
		"for ($i = 0; $i -lt 1; $i++) { sh -c x }",
		"while ($false) { sh -c x }",
		"do { sh -c x } while ($false)",
		"try { bash -c x } catch { sh -c x } finally { gc status }",
		"try { bash -c x } catch [System.Exception] { sh -c x }",
		"@('bash')",
		"[Console]::WriteLine('bash')",
		"param($x)\nWrite-Output $x",
		"[OutputType([string])]\nparam()\n'bash'",
		"using namespace System.Text\n[StringBuilder]::new()",
		"b'a'sh -c x",
		`ba""sh -c x`,
		"`#notcomment",
		"echo '# not comment'; bash",
		"echo \"ba`nsh\" | sh",
		"& { bash -c x }",
	}
	for _, shell := range powerShells(t) {
		t.Run(filepath.Base(shell), func(t *testing.T) {
			want := powerShellParserCommandSets(t, shell, inputs)
			if len(want) != len(inputs) {
				t.Fatalf("parser returned %d command sets for %d inputs", len(want), len(inputs))
			}
			for i, input := range inputs {
				got := sortedUnique(commandPrograms(input))
				if !reflect.DeepEqual(got, want[i]) {
					t.Errorf("commandPrograms(%q) = %q, %s parser reported %q", input, got, filepath.Base(shell), want[i])
				}
			}
		})
	}
}

func powerShellParserCommandSets(t *testing.T, shell string, inputs []string) [][]string {
	t.Helper()
	payload, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	payload = []byte(powerShellParserASCIIEscaped(string(payload)))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, shell, "-NoProfile", "-NonInteractive", "-Command", powerShellParserScript)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s parser command failed: %v\nstdout:\n%s\nstderr:\n%s", shell, err, stdout.String(), stderr.String())
	}
	var decoded []struct {
		Names []string `json:"Names"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &decoded); err != nil {
		t.Fatalf("decoding %s parser JSON: %v\nstdout:\n%s\nstderr:\n%s", shell, err, stdout.String(), stderr.String())
	}
	sets := make([][]string, len(decoded))
	for i, item := range decoded {
		sets[i] = sortedUnique(item.Names)
	}
	return sets
}

func powerShellParserASCIIEscaped(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < utf8.RuneSelf {
			b.WriteRune(r)
			continue
		}
		b.WriteString(fmt.Sprintf("\\u%04x", r))
	}
	return b.String()
}

func sortedUnique(values []string) []string {
	values = append([]string(nil), values...)
	slices.Sort(values)
	return slices.Compact(values)
}

const powerShellParserScript = `
$ErrorActionPreference = 'Stop'

function Get-ConstantCommandName {
    param($Ast)
    if ($null -eq $Ast) {
        return $null
    }
    if ($Ast -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
        return $Ast.Value
    }
    if ($Ast -is [System.Management.Automation.Language.ExpandableStringExpressionAst] -and $Ast.NestedExpressions.Count -eq 0) {
        return $Ast.Value
    }
    if ($Ast -is [System.Management.Automation.Language.ParenExpressionAst]) {
        $pipeline = $Ast.Pipeline
        if ($pipeline.PipelineElements.Count -eq 1 -and $pipeline.PipelineElements[0] -is [System.Management.Automation.Language.CommandExpressionAst]) {
            return Get-ConstantCommandName $pipeline.PipelineElements[0].Expression
        }
    }
    return $null
}

$commands = ConvertFrom-Json -InputObject ([Console]::In.ReadToEnd())
$results = @()
foreach ($command in $commands) {
    $tokens = $null
    $errors = $null
    $ast = [System.Management.Automation.Language.Parser]::ParseInput([string]$command, [ref]$tokens, [ref]$errors)
    $names = @()
    foreach ($commandAst in $ast.FindAll({ param($candidate) $candidate -is [System.Management.Automation.Language.CommandAst] }, $true)) {
        $name = $commandAst.GetCommandName()
        if ([string]::IsNullOrEmpty($name) -and $commandAst.CommandElements.Count -gt 0) {
            $name = Get-ConstantCommandName $commandAst.CommandElements[0]
        }
        if (![string]::IsNullOrEmpty($name)) {
            $names += $name
        }
    }
    $results += [pscustomobject]@{ Names = @($names | Sort-Object -Unique) }
}
[Console]::Out.Write((ConvertTo-Json -Compress -Depth 4 -InputObject $results))
`
