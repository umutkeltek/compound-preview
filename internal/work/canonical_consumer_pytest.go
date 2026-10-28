package work

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"path"
	"strings"
)

// pytest's xunit1 file/class/name tuple identifies parametrized tests without
// translating outcomes to another language's report.
func parsePytestIdentity(value string) ([]string, error) {
	var id []string
	if err := json.Unmarshal([]byte(value), &id); err != nil || len(id) != 3 {
		return nil, fmt.Errorf("must be [file,classname,name]")
	}
	for _, field := range id {
		if field == "" || strings.ContainsAny(field, "\x00\r\n") {
			return nil, fmt.Errorf("pytest identity fields must be nonempty single-line strings")
		}
	}
	if !validPytestFile(id[0]) {
		return nil, fmt.Errorf("pytest file must be clean repository-relative path")
	}
	canonical, err := json.Marshal(id)
	if err != nil || string(canonical) != value {
		return nil, fmt.Errorf("pytest identity must use canonical compact JSON")
	}
	return id, nil
}
func validPytestFile(file string) bool {
	return file != "" && len(file) <= 4096 && !path.IsAbs(file) && path.Clean(file) == file && file != "." && file != ".." &&
		!strings.HasPrefix(file, "../") && !strings.ContainsAny(file, "\\\x00\r\n")
}

var pytestAttrs = map[xml.Name]map[xml.Name]bool{
	{Local: "testsuites"}: {{Local: "name"}: true},
	{Local: "testsuite"}:  {{Local: "name"}: true, {Local: "errors"}: true, {Local: "failures"}: true, {Local: "skipped"}: true, {Local: "tests"}: true, {Local: "time"}: true, {Local: "timestamp"}: true, {Local: "hostname"}: true},
	{Local: "testcase"}:   {{Local: "classname"}: true, {Local: "name"}: true, {Local: "file"}: true, {Local: "line"}: true, {Local: "time"}: true},
	{Local: "skipped"}:    {{Local: "type"}: true, {Local: "message"}: true},
	{Local: "system-out"}: {}, {Local: "system-err"}: {},
}

func validPytestElement(node xml.StartElement) bool {
	allowed, ok := pytestAttrs[node.Name]
	if !ok || node.Name.Space != "" || len(node.Attr) > 64 {
		return false
	}
	seen := map[xml.Name]bool{}
	for _, a := range node.Attr {
		if a.Name.Space != "" || !allowed[a.Name] || seen[a.Name] || len(a.Value) > 64<<10 {
			return false
		}
		seen[a.Name] = true
	}
	return true
}
func validateConsumerPytestReport(c CanonicalConsumer, body []byte) error {
	d := xml.NewDecoder(bytes.NewReader(body))
	depth, roots, suites, total, skipped, declarations := 0, 0, 0, 0, 0, 0
	wantTests, wantSkipped := 0, 0
	seen := map[string]bool{}
	required, _ := consumerUniqueStrings(c.RequiredTests, true)
	allowed, _ := consumerUniqueStrings(c.AllowedSkips, false)
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("pytest JUnit: %w", err)
		}
		switch n := token.(type) {
		case xml.ProcInst:
			if n.Target != "xml" || roots != 0 || depth != 0 || declarations != 0 {
				return fmt.Errorf("invalid pytest XML declaration")
			}
			declarations++
		case xml.Directive, xml.Comment:
			return fmt.Errorf("pytest JUnit directive/comment is not native evidence")
		case xml.CharData:
			if strings.TrimSpace(string(n)) != "" {
				return fmt.Errorf("unexpected pytest JUnit text")
			}
		case xml.StartElement:
			depth++
			if !validPytestElement(n) {
				return fmt.Errorf("invalid pytest JUnit element/attributes")
			}
			switch depth {
			case 1:
				roots++
				if roots != 1 || n.Name != (xml.Name{Local: "testsuites"}) {
					return fmt.Errorf("pytest requires one testsuites root")
				}
			case 2:
				suites++
				if suites != 1 || n.Name != (xml.Name{Local: "testsuite"}) || xmlAttr(n.Attr, "name") == "" {
					return fmt.Errorf("pytest requires one named native testsuite")
				}
				counts := map[string]int{}
				for _, name := range []string{"tests", "failures", "errors", "skipped"} {
					v, err := xmlRequiredCount(n.Attr, name)
					if err != nil {
						return fmt.Errorf("pytest summary: %w", err)
					}
					counts[name] = *v
				}
				if counts["tests"] == 0 || counts["failures"] != 0 || counts["errors"] != 0 || counts["skipped"] > counts["tests"] {
					return fmt.Errorf("empty or failed pytest summary")
				}
				wantTests, wantSkipped = counts["tests"], counts["skipped"]
			case 3:
				if n.Name != (xml.Name{Local: "testcase"}) {
					return fmt.Errorf("unexpected pytest suite child")
				}
				tuple := []string{xmlAttr(n.Attr, "file"), xmlAttr(n.Attr, "classname"), xmlAttr(n.Attr, "name")}
				raw, err := json.Marshal(tuple)
				if err != nil {
					return err
				}
				key := string(raw)
				if _, err := parsePytestIdentity(key); err != nil {
					return err
				}
				if seen[key] {
					return fmt.Errorf("duplicate pytest testcase identity %s", key)
				}
				seen[key] = true
				total++
				wasSkipped, err := consumePytestCase(d)
				if err != nil {
					return err
				}
				depth--
				if wasSkipped {
					skipped++
					if required[key] || !allowed[key] {
						return fmt.Errorf("required or undeclared pytest test skipped: %s", key)
					}
				}
			default:
				return fmt.Errorf("non-native pytest nesting")
			}
		case xml.EndElement:
			if (depth == 2 && n.Name != (xml.Name{Local: "testsuite"})) || (depth == 1 && n.Name != (xml.Name{Local: "testsuites"})) || depth < 1 {
				return fmt.Errorf("invalid pytest close")
			}
			depth--
		}
	}
	if roots != 1 || suites != 1 || depth != 0 || total == 0 || total != wantTests || skipped != wantSkipped {
		return fmt.Errorf("incomplete or inconsistent pytest native report")
	}
	for id := range required {
		if !seen[id] {
			return fmt.Errorf("required pytest test absent: %s", id)
		}
	}
	return nil
}
func consumePytestCase(d *xml.Decoder) (bool, error) {
	skipped := false
	diagnostics := map[xml.Name]bool{}
	for {
		token, err := d.Token()
		if err != nil {
			return false, fmt.Errorf("unfinished pytest testcase: %w", err)
		}
		switch n := token.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(n)) != "" {
				return false, fmt.Errorf("unexpected pytest testcase text")
			}
		case xml.StartElement:
			if !validPytestElement(n) {
				return false, fmt.Errorf("invalid pytest outcome attributes")
			}
			switch n.Name {
			case xml.Name{Local: "skipped"}:
				if skipped || xmlAttr(n.Attr, "type") != "pytest.skip" {
					return false, fmt.Errorf("duplicate, unknown or xfail pytest skip")
				}
				skipped = true
			case xml.Name{Local: "system-out"}, xml.Name{Local: "system-err"}:
				if diagnostics[n.Name] {
					return false, fmt.Errorf("duplicate pytest diagnostic")
				}
				diagnostics[n.Name] = true
			default:
				return false, fmt.Errorf("failed or non-native pytest testcase outcome")
			}
			if err := consumePytestText(d, n.Name); err != nil {
				return false, err
			}
		case xml.EndElement:
			if n.Name != (xml.Name{Local: "testcase"}) {
				return false, fmt.Errorf("invalid pytest testcase close")
			}
			return skipped, nil
		default:
			return false, fmt.Errorf("invalid pytest testcase metadata")
		}
	}
}
func consumePytestText(d *xml.Decoder, name xml.Name) error {
	size := 0
	for {
		token, err := d.Token()
		if err != nil {
			return fmt.Errorf("unfinished pytest outcome: %w", err)
		}
		switch n := token.(type) {
		case xml.CharData:
			size += len(n)
			if size > 64<<10 {
				return fmt.Errorf("pytest diagnostic exceeds limit")
			}
		case xml.EndElement:
			if n.Name != name {
				return fmt.Errorf("invalid pytest outcome close")
			}
			return nil
		default:
			return fmt.Errorf("nested pytest outcomes/metadata refused")
		}
	}
}
