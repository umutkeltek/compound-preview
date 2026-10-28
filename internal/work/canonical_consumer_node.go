package work

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
)

// The tuple grammar is shared, not the report semantics. Node's suite counts
// refer to immediate children; Bun counts all descendant leaves.
func parseNodeTestIdentity(value string) (bunTestIdentity, error) {
	id, err := parseBunTestIdentity(value)
	if err != nil {
		return id, err
	}
	if path.IsAbs(id.File) || path.Clean(id.File) != id.File || id.File == "." || id.File == ".." || strings.HasPrefix(id.File, "../") || strings.ContainsAny(id.File, "\\\x00\r\n") {
		return id, fmt.Errorf("Node test file must be a clean repository-relative path")
	}
	return id, nil
}

type nodeSuite struct {
	name                      string
	children, skipped         int
	wantChildren, wantSkipped int
}

var (
	nodeRootName  = xml.Name{Local: "testsuites"}
	nodeSuiteName = xml.Name{Local: "testsuite"}
	nodeCaseName  = xml.Name{Local: "testcase"}
	nodeSkipName  = xml.Name{Local: "skipped"}
)

var nodeAttrs = map[xml.Name]map[xml.Name]bool{
	nodeRootName: {},
	nodeSuiteName: {
		{Local: "name"}: true, {Local: "time"}: true, {Local: "disabled"}: true, {Local: "errors"}: true,
		{Local: "tests"}: true, {Local: "failures"}: true, {Local: "skipped"}: true, {Local: "timestamp"}: true, {Local: "hostname"}: true,
	},
	nodeCaseName: {{Local: "name"}: true, {Local: "time"}: true, {Local: "classname"}: true, {Local: "file"}: true},
	nodeSkipName: {{Local: "type"}: true, {Local: "message"}: true},
}

func validNodeElement(node xml.StartElement) bool {
	allowed, ok := nodeAttrs[node.Name]
	if !ok || len(node.Attr) > 64 {
		return false
	}
	seen := map[xml.Name]bool{}
	for _, attr := range node.Attr {
		if !allowed[attr.Name] || len(attr.Value) > 64<<10 || seen[attr.Name] {
			return false
		}
		seen[attr.Name] = true
	}
	return true
}

func nodeReportFile(file, root string) (string, error) {
	if file == "" || !filepath.IsAbs(file) || filepath.Clean(file) != file || strings.ContainsAny(file, "\\\x00\r\n") {
		return "", fmt.Errorf("Node native test file must be a clean absolute path")
	}
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == "." || rel == ".." || filepath.IsAbs(rel) || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("Node native test file escapes sealed source root")
	}
	return filepath.ToSlash(rel), nil
}

func validateConsumerNodeReport(c CanonicalConsumer, body []byte, root string) error {
	if root == "" || len(root) > 4096 || !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.ContainsAny(root, "\\\x00\r\n") {
		return fmt.Errorf("runner-owned canonical Node source root required")
	}
	d := xml.NewDecoder(bytes.NewReader(body))
	d.Strict = true
	depth, roots, declarations, total := 0, 0, 0, 0
	var suites []nodeSuite
	seen := map[string]bool{}
	required, _ := consumerUniqueStrings(c.RequiredTests, true)
	allowed, _ := consumerUniqueStrings(c.AllowedSkips, false)
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("Node native JUnit: %w", err)
		}
		switch node := token.(type) {
		case xml.ProcInst:
			if node.Target != "xml" || roots != 0 || depth != 0 || declarations != 0 {
				return fmt.Errorf("invalid Node XML declaration")
			}
			declarations++
		case xml.Directive:
			return fmt.Errorf("Node JUnit directives are not native evidence")
		case xml.Comment:
			// Native test:diagnostic/log events are comments, not pass evidence.
			if depth == 0 || len(node) > 64<<10 {
				return fmt.Errorf("invalid Node diagnostic comment")
			}
		case xml.CharData:
			if strings.TrimSpace(string(node)) != "" {
				return fmt.Errorf("unexpected Node JUnit text")
			}
		case xml.StartElement:
			depth++
			if depth > 34 || !validNodeElement(node) {
				return fmt.Errorf("invalid Node JUnit structure or attributes")
			}
			if depth == 1 {
				roots++
				if roots != 1 || node.Name != nodeRootName {
					return fmt.Errorf("Node JUnit requires one testsuites root")
				}
				continue
			}
			switch node.Name {
			case nodeSuiteName:
				if depth != len(suites)+2 {
					return fmt.Errorf("invalid Node suite nesting")
				}
				name := xmlAttr(node.Attr, "name")
				if name == "" {
					return fmt.Errorf("Node suite name required")
				}
				counts := map[string]int{}
				for _, field := range []string{"tests", "failures", "errors", "skipped", "disabled"} {
					value, err := xmlRequiredCount(node.Attr, field)
					if err != nil {
						return err
					}
					counts[field] = *value
				}
				if counts["tests"] == 0 || counts["failures"] != 0 || counts["errors"] != 0 || counts["disabled"] != 0 {
					return fmt.Errorf("empty, failed or disabled Node suite")
				}
				if len(suites) > 0 {
					suites[len(suites)-1].children++
				}
				suites = append(suites, nodeSuite{name: name, wantChildren: counts["tests"], wantSkipped: counts["skipped"]})
			case nodeCaseName:
				if len(suites) == 0 || depth != len(suites)+2 {
					return fmt.Errorf("Node testcase must belong to a named native suite")
				}
				file, err := nodeReportFile(xmlAttr(node.Attr, "file"), root)
				if err != nil {
					return err
				}
				class, hasClass := xmlAttrOK(node.Attr, "classname")
				name := xmlAttr(node.Attr, "name")
				if !hasClass || name == "" {
					return fmt.Errorf("Node testcase requires classname and name")
				}
				id := bunTestIdentity{File: file, Classname: class, Name: name}
				for _, s := range suites {
					id.Suites = append(id.Suites, s.name)
				}
				key := bunIdentityString(id)
				if seen[key] {
					return fmt.Errorf("duplicate Node testcase identity %s", key)
				}
				seen[key] = true
				total++
				suites[len(suites)-1].children++
				skipped, err := consumeNodeTestcase(d)
				if err != nil {
					return err
				}
				depth--
				if skipped {
					suites[len(suites)-1].skipped++
					if required[key] || !allowed[key] {
						return fmt.Errorf("required or undeclared Node testcase skipped: %s", key)
					}
				}
			default:
				return fmt.Errorf("unexpected Node JUnit element")
			}
		case xml.EndElement:
			if node.Name == nodeSuiteName {
				if len(suites) == 0 {
					return fmt.Errorf("invalid Node suite close")
				}
				s := suites[len(suites)-1]
				suites = suites[:len(suites)-1]
				if s.children != s.wantChildren || s.skipped != s.wantSkipped {
					return fmt.Errorf("inconsistent Node immediate-child counts")
				}
				if s.skipped > 0 && len(suites) > 0 {
					suites[len(suites)-1].skipped++
				}
			} else if node.Name != nodeRootName || depth != 1 {
				return fmt.Errorf("invalid Node root close")
			}
			depth--
			if depth < 0 {
				return fmt.Errorf("invalid Node JUnit depth")
			}
		}
	}
	if roots != 1 || depth != 0 || len(suites) != 0 || total == 0 {
		return fmt.Errorf("Node JUnit requires a complete nonempty native report")
	}
	for id := range required {
		if !seen[id] {
			return fmt.Errorf("required Node testcase absent: %s", id)
		}
	}
	return nil
}

func consumeNodeTestcase(d *xml.Decoder) (bool, error) {
	skipped := false
	for {
		token, err := d.Token()
		if err != nil {
			return false, fmt.Errorf("unfinished Node testcase: %w", err)
		}
		switch node := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(node)) != "" {
				return false, fmt.Errorf("unexpected Node testcase text")
			}
		case xml.Comment:
			if len(node) > 64<<10 {
				return false, fmt.Errorf("oversized Node diagnostic")
			}
		case xml.StartElement:
			if node.Name != nodeSkipName || !validNodeElement(node) || skipped || xmlAttr(node.Attr, "type") != "skipped" {
				return false, fmt.Errorf("failed, TODO or invalid Node testcase outcome")
			}
			skipped = true
			for {
				token, err := d.Token()
				if err != nil {
					return false, err
				}
				if close, ok := token.(xml.EndElement); ok && close.Name == node.Name {
					break
				}
				if text, ok := token.(xml.CharData); !ok || strings.TrimSpace(string(text)) != "" {
					return false, fmt.Errorf("invalid Node skipped payload")
				}
			}
		case xml.EndElement:
			if node.Name != (xml.Name{Local: "testcase"}) {
				return false, fmt.Errorf("invalid Node testcase close")
			}
			return skipped, nil
		default:
			return false, fmt.Errorf("unexpected Node testcase metadata")
		}
	}
}
