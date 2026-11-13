package gitx

import (
	"errors"
	"strconv"
	"strings"
)

// strippedEnvPrefixes / strippedEnvKeys are removed from the inherited host
// environment so the host cannot redirect git operations, inject config, or
// trace (and thereby log) request headers. Matching is case-insensitive.
var strippedEnvPrefixes = []string{"GIT_CONFIG", "GIT_TRACE"}

var strippedEnvKeys = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_COMMON_DIR":                   true,
	"GIT_NAMESPACE":                    true,
	"GIT_CURL_VERBOSE":                 true,
	"GIT_ASKPASS":                      true,
	"SSH_ASKPASS":                      true,
	"GIT_TERMINAL_PROMPT":              true,
	"GCM_INTERACTIVE":                  true,
	"GIT_ALLOW_PROTOCOL":               true,
}

// safetyEnv is appended last so it overrides anything earlier.
var safetyEnv = []string{
	"GIT_TERMINAL_PROMPT=0",
	"GCM_INTERACTIVE=never",
	"GIT_ASKPASS=",
	"SSH_ASKPASS=",
	"GIT_ALLOW_PROTOCOL=https:http:file",
	"GIT_TRACE_REDACT=1",
}

func stripped(kv string) bool {
	k, _, _ := strings.Cut(kv, "=")
	k = strings.ToUpper(k)
	if strippedEnvKeys[k] {
		return true
	}
	for _, p := range strippedEnvPrefixes {
		if strings.HasPrefix(k, p) {
			return true
		}
	}
	return false
}

// buildEnv returns the child environment: base minus dangerous variables,
// then extra (Runner.Env), then the safety variables and one origin-scoped
// http.<origin>.extraHeader per authenticated http(s) remote, injected via
// GIT_CONFIG_COUNT/KEY/VALUE. Two remotes sharing an origin with different
// credentials are refused: callers must give each command only the
// credentials of the remote it talks to.
func buildEnv(base, extra []string, auth ...Remote) ([]string, error) {
	type header struct{ origin, value string }
	var headers []header
	for _, r := range auth {
		if r.Token == "" {
			continue
		}
		if strings.ContainsAny(r.Token, "\r\n\x00") || strings.ContainsAny(r.username(), ":\r\n\x00") {
			return nil, errors.New("gitx: credentials contain forbidden characters")
		}
		o := r.origin()
		if o == "" {
			continue
		}
		v := "Authorization: Basic " + r.basicAuth()
		dup := false
		for _, h := range headers {
			if h.origin != o {
				continue
			}
			if h.value != v {
				return nil, errors.New("gitx: refusing to send two different credentials to origin " + o)
			}
			dup = true
		}
		if !dup {
			headers = append(headers, header{o, v})
		}
	}

	env := make([]string, 0, len(base)+len(extra)+len(safetyEnv)+1+2*len(headers))
	for _, kv := range base {
		if !stripped(kv) {
			env = append(env, kv)
		}
	}
	env = append(env, extra...)
	env = append(env, safetyEnv...)
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(headers)))
	for i, h := range headers {
		n := strconv.Itoa(i)
		env = append(env, "GIT_CONFIG_KEY_"+n+"=http."+h.origin+".extraHeader", "GIT_CONFIG_VALUE_"+n+"="+h.value)
	}
	return env, nil
}
