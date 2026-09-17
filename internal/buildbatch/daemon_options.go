package buildbatch

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func validateQueryKeys(q url.Values, allowed ...string) error {
	allowedSet := make(map[string]struct{}, len(allowed))
	for _, key := range allowed {
		allowedSet[key] = struct{}{}
	}
	for key, values := range q {
		if _, ok := allowedSet[key]; !ok {
			return fmt.Errorf("unsupported query parameter %q", key)
		}
		if key == "var" {
			continue
		}
		if len(values) > 1 {
			return fmt.Errorf("query parameter %q must be specified at most once", key)
		}
	}
	return nil
}

func queryValue(q url.Values, key string) (string, bool, error) {
	values, ok := q[key]
	if !ok || len(values) == 0 {
		return "", false, nil
	}
	if len(values) > 1 {
		return "", false, fmt.Errorf("query parameter %q must be specified at most once", key)
	}
	return values[0], true, nil
}

func queryBoolStrict(q url.Values, key string, def bool) (bool, error) {
	v, ok, err := queryValue(q, key)
	if err != nil {
		return false, err
	}
	if !ok {
		return def, nil
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("query parameter %q must be a boolean", key)
	}
}

func queryIntStrict(q url.Values, key string, def int, min int) (int, error) {
	v, ok, err := queryValue(q, key)
	if err != nil {
		return 0, err
	}
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, fmt.Errorf("query parameter %q must be an integer", key)
	}
	if n < min {
		return 0, fmt.Errorf("query parameter %q must be greater than or equal to %d", key, min)
	}
	return n, nil
}

func queryDurationStrict(q url.Values, key string, def time.Duration, min time.Duration) (time.Duration, error) {
	v, ok, err := queryValue(q, key)
	if err != nil {
		return 0, err
	}
	if !ok || strings.TrimSpace(v) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return 0, fmt.Errorf("query parameter %q must be a duration", key)
	}
	if d < min {
		return 0, fmt.Errorf("query parameter %q must be greater than or equal to %s", key, min)
	}
	return d, nil
}

func buildOptionsFromQuery(q url.Values, defaultAddrs string, resultDB string, logsFile string) (options, error) {
	if err := validateQueryKeys(q, "addrs", "concurrency", "fail-fast", "oci", "both-formats", "oom-cooldown", "timeout", "retry", "verbose", "target", "skip-fail", "var", "oneshot"); err != nil {
		return options{}, err
	}
	addrsValue, ok, err := queryValue(q, "addrs")
	if err != nil {
		return options{}, err
	}
	if !ok || strings.TrimSpace(addrsValue) == "" {
		addrsValue = defaultAddrs
	}
	addrs, err := parseBuildkitAddrs(addrsValue)
	if err != nil {
		return options{}, err
	}
	concurrency, err := queryIntStrict(q, "concurrency", 1, 1)
	if err != nil {
		return options{}, err
	}
	timeout, err := queryIntStrict(q, "timeout", 300, 0)
	if err != nil {
		return options{}, err
	}
	retry, err := queryIntStrict(q, "retry", 0, 0)
	if err != nil {
		return options{}, err
	}
	oomCooldown, err := queryDurationStrict(q, "oom-cooldown", defaultBuildkitOOMCooldown, 0)
	if err != nil {
		return options{}, err
	}
	for _, addr := range addrs {
		if addr != nil {
			addr.cooldown = oomCooldown
		}
	}
	failFast, err := queryBoolStrict(q, "fail-fast", false)
	if err != nil {
		return options{}, err
	}
	oci, err := queryBoolStrict(q, "oci", false)
	if err != nil {
		return options{}, err
	}
	bothFormats, err := queryBoolStrict(q, "both-formats", false)
	if err != nil {
		return options{}, err
	}
	verbose, err := queryBoolStrict(q, "verbose", false)
	if err != nil {
		return options{}, err
	}
	skipFail, err := queryBoolStrict(q, "skip-fail", false)
	if err != nil {
		return options{}, err
	}
	target, _, err := queryValue(q, "target")
	if err != nil {
		return options{}, err
	}
	buildVars, err := parseBuildVariables(q["var"])
	if err != nil {
		return options{}, err
	}
	return options{
		addrs:       addrs,
		addrsRaw:    addrsValue,
		concurrency: concurrency,
		failfast:    failFast,
		oci:         oci,
		bothFormats: bothFormats,
		oomCooldown: oomCooldown,
		resultPath:  resultDB,
		logsPath:    logsFile,
		vars:        buildVars,
		timeout:     timeout,
		retry:       retry,
		verbose:     verbose,
		target:      strings.TrimSpace(target),
		skipFail:    skipFail,
	}, nil
}

func exportOptionsFromQuery(q url.Values, resultDB string) (options, error) {
	if err := validateQueryKeys(q, "oci", "with-fail"); err != nil {
		return options{}, err
	}
	oci, err := queryBoolStrict(q, "oci", false)
	if err != nil {
		return options{}, err
	}
	withFail, err := queryBoolStrict(q, "with-fail", false)
	if err != nil {
		return options{}, err
	}
	return options{
		fromResultPath: resultDB,
		oci:            oci,
		withFail:       withFail,
	}, nil
}

func preheatOptionsFromQuery(q url.Values, resultDB string) (options, error) {
	if err := validateQueryKeys(q, "dragonfly-scheduler-addr", "concurrency", "interval", "timeout", "fail-fast", "oci", "verbose"); err != nil {
		return options{}, err
	}
	schedulerAddr, ok, err := queryValue(q, "dragonfly-scheduler-addr")
	if err != nil {
		return options{}, err
	}
	if !ok || strings.TrimSpace(schedulerAddr) == "" {
		return options{}, fmt.Errorf("dragonfly-scheduler-addr is required")
	}
	concurrency, err := queryIntStrict(q, "concurrency", 1, 1)
	if err != nil {
		return options{}, err
	}
	interval, err := queryIntStrict(q, "interval", 5, 0)
	if err != nil {
		return options{}, err
	}
	timeout, err := queryIntStrict(q, "timeout", 5, 0)
	if err != nil {
		return options{}, err
	}
	failFast, err := queryBoolStrict(q, "fail-fast", false)
	if err != nil {
		return options{}, err
	}
	oci, err := queryBoolStrict(q, "oci", false)
	if err != nil {
		return options{}, err
	}
	verbose, err := queryBoolStrict(q, "verbose", false)
	if err != nil {
		return options{}, err
	}
	return options{
		fromResultPath:         resultDB,
		dragonflySchedulerAddr: strings.TrimSpace(schedulerAddr),
		concurrency:            concurrency,
		interval:               interval,
		timeout:                timeout,
		failfast:               failFast,
		oci:                    oci,
		verbose:                verbose,
	}, nil
}
