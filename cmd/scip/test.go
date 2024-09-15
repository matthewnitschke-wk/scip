package main

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/fatih/color"
	"github.com/sourcegraph/scip/bindings/go/scip"
	"github.com/urfave/cli/v2"
)

type testFlags struct {
	from          string // default: 'index.scip'
	commentSyntax string // default: '//'
	pathFilters   cli.StringSlice
}

func testCommand() cli.Command {
	var testFlags testFlags
	test := cli.Command{
		Name:  "test",
		Usage: "Validate subsets of snapshot files",
		Description: `The test subcommand validates whether a provided SCIP index
matches manually specified symbol fields. Refer to
https://github.com/sourcegraph/scip/blob/main/docs/test_command.md for documentation
on the expected file format of the test files.`,
		Flags: []cli.Flag{
			fromFlag(&testFlags.from),
			commentSyntaxFlag(&testFlags.commentSyntax),
			&cli.StringSliceFlag{
				Name:        "filter",
				Aliases:     []string{"f"},
				Usage:       "Filter the test files to run. Only test files that match the provided filter(s).",
				Destination: &testFlags.pathFilters,
			},
		},
		Action: func(c *cli.Context) error {
			dir := c.Args().Get(0)

			index, err := readFromOption(testFlags.from)
			if err != nil {
				return err
			}

			return ExecuteTestCommand(dir, testFlags.pathFilters.Value(), index, testFlags.commentSyntax)
		},
	}
	return test
}

func ExecuteTestCommand(
	directory string,
	fileFilters []string,
	index *scip.Index,
	commentSyntax string,
) error {
	hasFailure := false

	for _, document := range index.Documents {
		sourceFilePath := filepath.Join(directory, document.RelativePath)

		if len(fileFilters) > 0 {
			if !slices.Contains(fileFilters, document.RelativePath) {
				continue
			}
		}

		data, err := os.ReadFile(sourceFilePath)
		if err != nil {
			return err
		}

		failures := []string{}
		successCount := 0

		lines := strings.Split(string(data), "\n")
		for lineNumber := 0; lineNumber < len(lines); lineNumber++ {
			testCasesAtLine, usedLines := testCasesForLine(lineNumber, lines, commentSyntax)

			// if the test file contains no test lines, skip it. Only test the lines
			// that the test file dictates should be tested
			if len(testCasesAtLine) == 0 {
				continue
			}

			attributes := attributesForOccurrencesAtLine(lineNumber, document.Occurrences)
			for _, testCase := range testCasesAtLine {
				filteredAttrs := filterAttributesForTestCase(testCase, attributes)
				if !testCase.checkAll(filteredAttrs) {
					failures = append(failures, formatFailure(lineNumber, testCase, filteredAttrs))
				} else {
					successCount++
				}
			}

			lineNumber += usedLines
		}

		if len(failures) > 0 {
			hasFailure = true
			red := color.New(color.FgRed)
			red.Printf("✗ %s\n", document.RelativePath)

			for _, failure := range failures {
				fmt.Println(indent(failure, 4))
			}
		} else {
			green := color.New(color.FgGreen)
			green.Printf("✓ %s (%d assertions)\n", document.RelativePath, successCount)
		}
	}

	if hasFailure {
		return cli.Exit("", 1)
	}

	return nil
}

// symbolAttribute refers to a single attribute of a symbol.
// This can be a definition, reference, documentation, or diagnostic
type symbolAttribute struct {
	// the column number where this symbol starts
	start int

	// the length of the symbol's name
	length int

	// the type of attribute that this is
	kind symbolAttributeKind

	// contextual information about the attribute, as determined
	// by the [kind]
	data string

	// any additional information necessary to represent this attribute
	// each line should be considered a "newline", and is used for multiline
	// comments (in documentation), and diagnostic information
	additionalData []string
}

type symbolAttributeKind string

const (
	definitionAttrKind        symbolAttributeKind = "definition"
	referenceAttrKind         symbolAttributeKind = "reference"
	forwardDefinitionAttrKind symbolAttributeKind = "forward_definition"
	diagnosticAttrKind        symbolAttributeKind = "diagnostic"
)

func symbolAttributeKindFromStr(str string) symbolAttributeKind {
	switch str {
	case "definition":
		return definitionAttrKind
	case "reference":
		return referenceAttrKind
	case "forward_definition":
		return forwardDefinitionAttrKind
	case "diagnostic":
		return diagnosticAttrKind
	default:
		panic(fmt.Sprintf("Unknown symbolAttributeKind: %s", str))
	}
}

// symbolAttributeTestCase refers to metadata used to validate
// [symbolAttributes]
type symbolAttributeTestCase struct {
	attribute     symbolAttribute
	enforceLength bool
}

// commentsForLine returns the list of lines, after a provided [lineNumber], which are
// classified as comment. The comment type can be configured using [commentSyntax].
//
// Returns the list of symbolAttributeTestCase(s) for the provided line, and the number of
// of lines that were "consumed" by the cases on this line
func testCasesForLine(lineNumber int, lines []string, commentSyntax string) ([]symbolAttributeTestCase, int) {
	testCases := []symbolAttributeTestCase{}

	// if the specified lineNumber is outside the bounds of lines
	// return an empty array
	if lineNumber >= len(lines)-1 {
		return testCases, 0
	}

	testLines := []symbolAttributeTestCase{}
	usedLines := 0
	for i := lineNumber + 1; i < len(lines); i++ {
		line := lines[i]

		if !strings.HasPrefix(strings.TrimSpace(line), commentSyntax) {
			break
		}
		testCase := parseTestCase(line, lines[i+1:], commentSyntax)

		testLines = append(testLines, testCase)
		i += len(testCase.attribute.additionalData)
		usedLines += 1 + len(testCase.attribute.additionalData)
	}

	return testLines, usedLines
}

func filterAttributesForTestCase(testCase symbolAttributeTestCase, attributes []symbolAttribute) []symbolAttribute {
	filteredAttrs := []symbolAttribute{}
	for _, attr := range attributes {
		if testCase.attribute.start >= attr.start && testCase.attribute.start <= (attr.start+attr.length)-1 {
			filteredAttrs = append(filteredAttrs, attr)
		}
	}
	return filteredAttrs
}

func attributesForOccurrencesAtLine(lineNumber int, occurrences []*scip.Occurrence) []symbolAttribute {
	result := []symbolAttribute{}
	for _, occ := range occurrences {
		if occ.Range[0] == int32(lineNumber) {
			pos, _ := scip.NewRange(occ.Range)

			start := int(pos.Start.Character)
			length := int(pos.End.Character - pos.Start.Character)

			kind := referenceAttrKind
			if scip.SymbolRole_Definition.Matches(occ) {
				kind = definitionAttrKind
			} else if scip.SymbolRole_ForwardDefinition.Matches(occ) {
				kind = forwardDefinitionAttrKind
			}
			result = append(result, symbolAttribute{
				start:          start,
				length:         length,
				kind:           kind,
				data:           occ.Symbol,
				additionalData: []string{},
			})

			for _, diagnostic := range occ.Diagnostics {
				result = append(result, symbolAttribute{
					start:  start,
					length: length,
					kind:   diagnosticAttrKind,
					data:   diagnostic.Severity.String(),
					additionalData: []string{
						diagnostic.Message,
					},
				})
			}
		}
	}
	return result
}

func parseTestCase(line string, leadingLines []string, commentSyntax string) symbolAttributeTestCase {
	start := 0
	length := 0
	enforceLength := false

	if strings.Contains(line, "<-") {
		// if the test line selects via `<-`, treat the symbol selection
		// as the location of the commentSyntax
		start = strings.Index(line, commentSyntax)
		line = strings.Replace(line, "<-", "", 1)
	} else {
		// otherwise treat the start as the first `^`
		start = strings.Index(line, "^")

		// a single `^` dictates no length enforcement
		// anything more signifies length should be verified
		if strings.Contains(line, "^^") {
			enforceLength = true
			length = strings.Count(line, "^")
		}
		line = strings.ReplaceAll(line, "^", "")
	}

	// remove the comment prefix & whitespace
	line = strings.TrimSpace(strings.Replace(line, commentSyntax, "", 1))

	// the type of the symbol should be the first word
	// this is "definition", "reference", "documentation", "diagnostic", etc..
	kindStr := strings.Split(line, " ")[0]

	// the data is everything except the type
	data := strings.TrimSpace(strings.Replace(line, kindStr, "", 1))

	additionalData := []string{}
	for i := range leadingLines {
		leadingLine := leadingLines[i]
		// if the leadingLine is not a comment line, we're outside of the test case block.
		if !strings.HasPrefix(strings.TrimSpace(leadingLine), commentSyntax) {
			break
		}

		// remove the comment character(s)
		leadingLine = strings.Replace(leadingLine, commentSyntax, "", 1)

		// if the leading line doesn't start with '>', its not a multiline case
		if !strings.HasPrefix(strings.TrimSpace(leadingLine), ">") {
			break
		}

		// remove the '>' character
		leadingLine = strings.Replace(leadingLine, ">", "", 1)
		additionalData = append(additionalData, strings.TrimSpace(leadingLine))
	}

	return symbolAttributeTestCase{
		attribute: symbolAttribute{
			kind:           symbolAttributeKindFromStr(kindStr),
			start:          start,
			length:         length,
			data:           data,
			additionalData: additionalData,
		},
		enforceLength: enforceLength,
	}
}

func (s symbolAttributeTestCase) checkAll(attributes []symbolAttribute) bool {
	for _, attr := range attributes {
		if s.check(attr) {
			return true
		}
	}
	return false
}

func (s symbolAttributeTestCase) check(attr symbolAttribute) bool {
	if s.enforceLength {
		if s.attribute.length != attr.length || s.attribute.start != attr.start {
			return false
		}
	} else {
		if s.attribute.start < attr.start || s.attribute.start > (attr.start+attr.length)-1 {
			return false
		}
	}

	if s.attribute.kind != attr.kind {
		return false
	}

	// check if symbols are equal, a `.` character in the testCaseSymbol is considered
	// a wildcard, and matches the correlating group
	testCaseSymbolParts := strings.Split(s.attribute.data, " ")
	attrSymbolParts := strings.Split(attr.data, " ")
	for i, testCaseSymbolPart := range testCaseSymbolParts {
		if testCaseSymbolPart == "." {
			continue
		}
		if testCaseSymbolPart != attrSymbolParts[i] {
			return false
		}
	}

	// only validate additionalData if the testCases provides one
	// otherwise, ignore what the attribute specifies
	if len(s.attribute.additionalData) > 0 {
		if !slices.Equal(s.attribute.additionalData, attr.additionalData) {
			return false
		}
	}

	return true
}

func formatFailure(lineNumber int, testCase symbolAttributeTestCase, attributesAtLine []symbolAttribute) string {
	failureDesc := []string{
		fmt.Sprintf("Failure - row: %d, column: %d", lineNumber, testCase.attribute.start),
		fmt.Sprintf("  Expected: '%s %s'", testCase.attribute.kind, testCase.attribute.data),
	}
	for _, add := range testCase.attribute.additionalData {
		failureDesc = append(failureDesc, indent(fmt.Sprintf("'%s'", add), 12))
	}

	failureDesc = append(failureDesc, "  Actual:")
	if (len(attributesAtLine)) == 0 {
		failureDesc = append(failureDesc, "    - No attributes found")
	} else {
		for _, attr := range attributesAtLine {
			failureDesc = append(failureDesc, fmt.Sprintf("    - '%s %s'", attr.kind, attr.data))
			for _, add := range attr.additionalData {
				failureDesc = append(failureDesc, indent(fmt.Sprintf("'%s'", add), 6))
			}
		}
	}
	return strings.Join(failureDesc, "\n")
}

// --------------------------------- Utils ---------------------------------

func indent(str string, count int) string {
	lines := strings.Split(str, "\n")
	newLines := []string{}
	for _, line := range lines {
		newLines = append(newLines, strings.Repeat(" ", count)+line)
	}
	return strings.Join(newLines, "\n")
}
