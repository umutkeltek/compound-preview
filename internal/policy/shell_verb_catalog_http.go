package policy

import (
	"net/url"
	"strings"
)

// HTTP-client rows match a request by where it goes and how, not by
// subcommand words: every URL must be on the row's host, and the method must
// be one the row lists. The parse is deliberately partial and fails toward the
// legacy classifier -- an option whose arity it does not know turns its value
// into a would-be URL on some other host, and the row does not match.

var shellHTTPClientBinaries = map[string]bool{"curl": true, "http": true, "https": true, "xh": true}

type shellHTTPRequest struct {
	hosts  []string
	method string
	// readSafe is false when the request carries a body, chains a second
	// request, or writes a local file: such a request is not a pure read even
	// when its method is GET.
	readSafe bool
}

func (row ShellVerbCatalogRow) matchHTTP(binary string, args []string) (string, bool) {
	var request shellHTTPRequest
	var ok bool
	if binary == "curl" {
		request, ok = parseCurlRequest(args)
	} else {
		request, ok = parseHTTPieRequest(args)
	}
	if !ok || len(request.hosts) == 0 {
		return "", false
	}
	for _, host := range request.hosts {
		if host != row.URLHost {
			return "", false
		}
	}
	if row.MutationClass == "read" && !request.readSafe {
		return "", false
	}
	for _, method := range row.Methods {
		if method == "*" || method == request.method {
			return row.destination(nil, row.URLHost, false), true
		}
	}
	return "", false
}

// shellHTTPHost returns the lowercased host a URL operand names, defaulting the
// scheme the way the clients do. Userinfo cannot spoof it: url.Parse puts
// a trusted host name placed before an `@` into userinfo, so the real host
// (the part after the `@`) is what gets compared.
func shellHTTPHost(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || strings.Contains(raw, shellSubstitutionPlaceholder) {
		return "", false
	}
	if strings.HasPrefix(raw, ":") {
		return "localhost", true // HTTPie's :port/path shorthand
	}
	if !strings.Contains(raw, "://") {
		raw = "http://" + raw
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" {
		return "", false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
	default:
		return "", false
	}
	return strings.ToLower(parsed.Hostname()), true
}

var (
	// curlShortValue are curl's short options that take an argument.
	curlShortValue = "AbcCdDeEFHKmoPQrtTuUwxXYyz"
	curlLongValue  = map[string]bool{
		"--request": true, "--header": true, "--user": true, "--user-agent": true, "--referer": true,
		"--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true, "--data-ascii": true,
		"--json": true, "--form": true, "--form-string": true, "--upload-file": true, "--url": true, "--url-query": true,
		"--output": true, "--output-dir": true, "--dump-header": true, "--cookie": true, "--cookie-jar": true,
		"--config": true, "--max-time": true, "--connect-timeout": true, "--retry": true, "--retry-delay": true,
		"--retry-max-time": true, "--write-out": true, "--oauth2-bearer": true, "--range": true, "--time-cond": true,
		"--cacert": true, "--capath": true, "--cert": true, "--cert-type": true, "--key": true, "--key-type": true,
		"--limit-rate": true, "--speed-limit": true, "--speed-time": true, "--interface": true, "--stderr": true,
		"--trace": true, "--trace-ascii": true, "--libcurl": true, "--etag-save": true, "--etag-compare": true,
		"--hsts": true, "--alt-svc": true, "--proxy": true, "--preproxy": true, "--proxy-user": true,
		"--resolve": true, "--connect-to": true, "--unix-socket": true, "--abstract-unix-socket": true,
		"--doh-url": true, "--variable": true, "--expand-url": true, "--expand-data": true, "--expand-header": true,
	}
	// curlRetarget options can send the request somewhere other than the URL
	// the command shows, or read options from a file: the host is not known.
	curlRetarget = map[string]bool{
		"--proxy": true, "-x": true, "--preproxy": true, "--resolve": true, "--connect-to": true,
		"--unix-socket": true, "--abstract-unix-socket": true, "--doh-url": true, "--config": true, "-K": true,
		"--expand-url": true, "--variable": true,
	}
	curlBody = map[string]bool{
		"-d": true, "--data": true, "--data-raw": true, "--data-binary": true, "--data-urlencode": true,
		"--data-ascii": true, "--json": true, "-F": true, "--form": true, "--form-string": true,
		"--expand-data": true,
	}
	// curlLocalWrite options write a local file or start a second request.
	curlLocalWrite = map[string]bool{
		"-o": true, "--output": true, "--output-dir": true, "-O": true, "--remote-name": true,
		"--remote-name-all": true, "-J": true, "--remote-header-name": true, "-D": true, "--dump-header": true,
		"-c": true, "--cookie-jar": true, "--trace": true, "--trace-ascii": true, "--libcurl": true,
		"--create-dirs": true, "--stderr": true, "--etag-save": true, "--hsts": true, "--alt-svc": true,
		"-:": true, "--next": true, "-T": true, "--upload-file": true,
	}
)

func parseCurlRequest(args []string) (shellHTTPRequest, bool) {
	request := shellHTTPRequest{readSafe: true}
	explicitMethod, head, get, body, upload := "", false, false, false, false
	note := func(option, value string) bool {
		if curlRetarget[option] {
			return false
		}
		if curlBody[option] {
			body = true
		}
		if curlLocalWrite[option] {
			request.readSafe = false
		}
		switch option {
		case "-X", "--request":
			explicitMethod = strings.ToUpper(value)
		case "-I", "--head":
			head = true
		case "-G", "--get":
			get = true
		case "-T", "--upload-file":
			upload = true
		case "--url":
			host, ok := shellHTTPHost(value)
			if !ok {
				return false
			}
			request.hosts = append(request.hosts, host)
		}
		return true
	}
	positional := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case !positional && arg == "--":
			positional = true
		case !positional && strings.HasPrefix(arg, "--"):
			if strings.Contains(arg, "=") {
				return shellHTTPRequest{}, false // curl has no --opt=value form
			}
			value := ""
			if curlLongValue[arg] {
				if i+1 >= len(args) {
					return shellHTTPRequest{}, false
				}
				i++
				value = args[i]
			}
			if !note(arg, value) {
				return shellHTTPRequest{}, false
			}
		case !positional && strings.HasPrefix(arg, "-") && len(arg) > 1:
			// Short options bundle (`-sSL`); a value option ends the bundle and
			// takes the rest of the word or the next word (`-XPOST`, `-X POST`).
			letters := arg[1:]
			for j := 0; j < len(letters); j++ {
				option := "-" + string(letters[j])
				value := ""
				if strings.ContainsRune(curlShortValue, rune(letters[j])) {
					if value = letters[j+1:]; value == "" {
						if i+1 >= len(args) {
							return shellHTTPRequest{}, false
						}
						i++
						value = args[i]
					}
					j = len(letters)
				}
				if !note(option, value) {
					return shellHTTPRequest{}, false
				}
			}
		default:
			host, ok := shellHTTPHost(arg)
			if !ok {
				return shellHTTPRequest{}, false
			}
			request.hosts = append(request.hosts, host)
		}
	}
	switch {
	case explicitMethod != "":
		request.method = explicitMethod
	case head:
		request.method = "HEAD"
	case upload:
		request.method = "PUT"
	case body && !get:
		request.method = "POST"
	default:
		request.method = "GET"
	}
	if body && !get {
		request.readSafe = false
	}
	return request, true
}

var (
	httpieValue = map[string]bool{
		"--auth": true, "-a": true, "--auth-type": true, "-A": true, "--output": true, "-o": true,
		"--session": true, "--session-read-only": true, "--verify": true, "--cert": true, "--cert-key": true,
		"--cert-key-pass": true, "--proxy": true, "--timeout": true, "--print": true, "-p": true,
		"--pretty": true, "--style": true, "-s": true, "--format-options": true, "--max-redirects": true,
		"--default-scheme": true, "--response-charset": true, "--response-mime": true, "--unix-socket": true,
		"--boundary": true, "--ciphers": true, "--ssl": true, "--history-print": true, "-P": true,
		"--raw": true, "--resolve": true, "--interface": true,
	}
	httpieRetarget = map[string]bool{"--proxy": true, "--unix-socket": true, "--resolve": true, "--default-scheme": true}
	httpieBody     = map[string]bool{"--form": true, "-f": true, "--multipart": true, "--raw": true}
	httpieLocal    = map[string]bool{
		"--output": true, "-o": true, "--download": true, "-d": true, "--continue": true, "-c": true,
		"--session": true,
	}
)

// parseHTTPieRequest reads `http|https|xh [flags] [METHOD] URL [ITEM...]`.
// Header (`Name:value`) and query (`name==value`) items keep a request a read;
// any data or file item (`=`, `:=`, `@`) makes it a body, as HTTPie does.
func parseHTTPieRequest(args []string) (shellHTTPRequest, bool) {
	request := shellHTTPRequest{readSafe: true}
	body := false
	var positionals []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			positionals = append(positionals, arg)
			continue
		}
		name, _, attached := strings.Cut(arg, "=")
		if httpieRetarget[name] {
			return shellHTTPRequest{}, false
		}
		if httpieBody[name] {
			body = true
		}
		if httpieLocal[name] {
			request.readSafe = false
		}
		if httpieValue[name] && !attached {
			if i+1 >= len(args) {
				return shellHTTPRequest{}, false
			}
			i++
		}
	}
	if len(positionals) == 0 {
		return shellHTTPRequest{}, false
	}
	method := ""
	if len(positionals) > 1 && shellHTTPMethodWord(positionals[0]) {
		method, positionals = strings.ToUpper(positionals[0]), positionals[1:]
	}
	host, ok := shellHTTPHost(positionals[0])
	if !ok {
		return shellHTTPRequest{}, false
	}
	request.hosts = []string{host}
	for _, item := range positionals[1:] {
		if !httpieItemIsHeaderOrQuery(item) {
			body = true
		}
	}
	if method == "" {
		method = "GET"
		if body {
			method = "POST"
		}
	}
	request.method = method
	if body {
		request.readSafe = false
	}
	return request, true
}

func shellHTTPMethodWord(word string) bool {
	if word == "" || len(word) > 10 {
		return false
	}
	for _, r := range word {
		if !(r >= 'A' && r <= 'Z') && !(r >= 'a' && r <= 'z') {
			return false
		}
	}
	return true
}

// httpieItemIsHeaderOrQuery classifies a request item by its first separator.
// An escaped separator is not interpreted here; such an item counts as data,
// which only ever makes the request look less like a read.
func httpieItemIsHeaderOrQuery(item string) bool {
	if strings.Contains(item, `\`) {
		return false
	}
	index := strings.IndexAny(item, ":=@")
	if index <= 0 {
		return false
	}
	rest := item[index:]
	switch {
	case strings.HasPrefix(rest, "=="):
		return true
	case strings.HasPrefix(rest, ":="):
		return false
	case rest[0] == ':':
		return true
	default:
		return false
	}
}
