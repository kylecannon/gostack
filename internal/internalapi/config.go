package internalapi

import (
	"fmt"
	"math"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

var variablePattern = regexp.MustCompile(`^[_a-zA-Z0-9]+$`)

// configUpdate is a validated config update request.
type configUpdate struct {
	variable string
	value    any
}

// getConfig answers GET /_gostack/config and GET /_localstack/config with every
// setting by its LocalStack name.
func (a *api) getConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, a.deps.Config.Values())
}

// localstackUpdateConfig answers POST /_localstack/config. An invalid request is
// a 400 with the body {}, as in LocalStack; a body the in-flight bound has no
// room for is a 503 (writeBusy), as for health.
func (a *api) localstackUpdateConfig(w http.ResponseWriter, r *http.Request) {
	body, busy := decodeObject(r)
	if busy {
		writeBusy(w)

		return
	}

	update, ok := parseConfigUpdate(body)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{})

		return
	}
	err := a.applyConfigUpdate(update)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{})

		return
	}
	a.writeConfigValue(w, update.variable)
}

// gostackUpdateConfig answers POST and PATCH /_gostack/config, with the same
// request and response bodies as the LocalStack alias, and a message in the 400.
func (a *api) gostackUpdateConfig(w http.ResponseWriter, r *http.Request) {
	body, busy := decodeObject(r)
	if busy {
		writeBusy(w)

		return
	}

	update, ok := parseConfigUpdate(body)
	if !ok {
		writeError(w, http.StatusBadRequest, `the body must be {"variable": "<NAME>", "value": <value>}`)

		return
	}
	err := a.applyConfigUpdate(update)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())

		return
	}
	a.writeConfigValue(w, update.variable)
}

// parseConfigUpdate reads {"variable": ..., "value": ...}. The variable must be
// made of letters, digits and underscores. A number written as a string counts as
// a number, as in LocalStack, where is_number converts it to a float. body is
// the request's, as decodeObject returns it.
func parseConfigUpdate(body map[string]any) (configUpdate, bool) {
	variable, _ := body["variable"].(string)
	if !variablePattern.MatchString(variable) {
		return configUpdate{}, false
	}

	return configUpdate{variable: variable, value: numberFromString(body["value"])}, true
}

func numberFromString(value any) any {
	text, isString := value.(string)
	if !isString {
		return value
	}
	number, err := strconv.ParseFloat(strings.TrimSpace(text), 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) {
		return value
	}

	return number
}

func (a *api) applyConfigUpdate(update configUpdate) error {
	err := a.deps.Config.Set(update.variable, update.value)
	if err != nil {
		return fmt.Errorf("update %s: %w", update.variable, err)
	}

	return nil
}

// writeConfigValue answers an update with the variable's value now in force.
func (a *api) writeConfigValue(w http.ResponseWriter, variable string) {
	writeJSON(w, http.StatusOK, map[string]any{
		"variable": variable,
		"value":    a.deps.Config.Value(variable),
	})
}
