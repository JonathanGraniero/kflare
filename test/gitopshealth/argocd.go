/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

// Package gitopshealth judges a kflare resource's health as Argo CD and Flux do, with the health checks kflare
// documents for them: the Lua script in config/argocd/health.lua and the CEL expressions in
// config/flux/health-check-exprs.yaml. Their tests and the controllers' shared specs use it, so that both checks are
// tested on the objects kflare writes, the way the tools run them.
package gitopshealth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	lua "github.com/yuin/gopher-lua"
)

// The health statuses of Argo CD, which a health check returns.
const (
	ArgoCDHealthy     = "Healthy"
	ArgoCDProgressing = "Progressing"
	ArgoCDDegraded    = "Degraded"
	ArgoCDSuspended   = "Suspended"
	ArgoCDMissing     = "Missing"
	ArgoCDUnknown     = "Unknown"
)

// ArgoCDHealth is the health Argo CD gives a resource: one of the statuses above, and a message.
type ArgoCDHealth struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// argoCDInvalidStatus is Argo CD's message for a health check that returns a status it does not know
// (util/lua's invalidHealthStatus).
const argoCDInvalidStatus = "Lua returned an invalid health status"

// argoCDScriptTimeout is how long Argo CD lets a health check run.
const argoCDScriptTimeout = time.Second

// RunArgoCDHealthScript runs the health check script on obj as Argo CD runs one from argocd-cm, and returns the
// health it gives. It follows util/lua's VM.ExecuteHealthLua in Argo CD v3.5.6, which embeds gopher-lua v1.1.2, the
// version this module requires:
//   - Lua's standard libraries are closed but for package, base and table, as for a script that argocd-cm's useOpenLibs
//     does not open them for. Argo CD also opens a safe subset of os, which kflare's script does not use: a script
//     that calls it fails here.
//   - The script gets obj as the global obj, converted as util/lua's decodeValue converts it, and one second to run.
//   - It returns a table holding status and message, nil (no health), or anything else, an error. A status Argo CD
//     does not know gives Unknown, with Argo CD's message.
//
// obj is an object as the API server returns it, decoded into unstructured content, so that its integers are int64.
func RunArgoCDHealthScript(script string, obj map[string]any) (ArgoCDHealth, error) {
	l := lua.NewState(lua.Options{SkipOpenLibs: true})
	defer l.Close()
	for _, lib := range []struct {
		name string
		open lua.LGFunction
	}{
		{lua.LoadLibName, lua.OpenPackage},
		{lua.BaseLibName, lua.OpenBase},
		{lua.TabLibName, lua.OpenTable},
	} {
		if err := l.CallByParam(lua.P{Fn: l.NewFunction(lib.open), NRet: 0, Protect: true},
			lua.LString(lib.name)); err != nil {
			return ArgoCDHealth{}, fmt.Errorf("opening Lua's %s library: %w", lib.name, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), argoCDScriptTimeout)
	defer cancel()
	l.SetContext(ctx)
	l.SetGlobal("obj", luaValue(l, obj))
	if err := l.DoString(script); err != nil {
		return ArgoCDHealth{}, err
	}

	switch result := l.Get(-1).(type) {
	case *lua.LTable:
		return argoCDHealthOf(result)
	case *lua.LNilType:
		return ArgoCDHealth{}, nil
	default:
		return ArgoCDHealth{}, fmt.Errorf("expect table output from Lua script, not %s", result.Type())
	}
}

// argoCDHealthOf reads the table a health check returned, as Argo CD does by encoding it to JSON (with
// layeh.com/gopher-json) and decoding that into its HealthStatus: an empty table, which encodes to an array, gives an
// empty health, a table that does not encode fails, and so does a status or message that is not a string.
func argoCDHealthOf(table *lua.LTable) (ArgoCDHealth, error) {
	value, err := jsonValue(table)
	if err != nil {
		return ArgoCDHealth{}, err
	}
	if _, isArray := value.([]any); isArray {
		return ArgoCDHealth{}, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ArgoCDHealth{}, err
	}
	var health ArgoCDHealth
	if err := json.Unmarshal(encoded, &health); err != nil {
		return ArgoCDHealth{}, fmt.Errorf("reading the health check's result: %w", err)
	}
	switch health.Status {
	case ArgoCDHealthy, ArgoCDProgressing, ArgoCDDegraded, ArgoCDSuspended, ArgoCDMissing, ArgoCDUnknown:
		return health, nil
	default:
		return ArgoCDHealth{Status: ArgoCDUnknown, Message: argoCDInvalidStatus}, nil
	}
}

// jsonValue converts a value of a Lua table to what gopher-json encodes it as: a table whose keys are all strings to
// an object, an empty one or one whose keys are 1 to n to an array, and any other table, a function or userdata to an
// error. A table holds no nil value.
func jsonValue(value lua.LValue) (any, error) {
	switch v := value.(type) {
	case lua.LString:
		return string(v), nil
	case lua.LNumber:
		return float64(v), nil
	case lua.LBool:
		return bool(v), nil
	case *lua.LTable:
		return jsonTable(v)
	default:
		return nil, fmt.Errorf("cannot encode %s to JSON", value.Type())
	}
}

// jsonTable converts a Lua table for jsonValue.
func jsonTable(table *lua.LTable) (any, error) {
	object, array := map[string]any{}, []any{}
	var err error
	table.ForEach(func(key, item lua.LValue) {
		if err != nil {
			return
		}
		var converted any
		if converted, err = jsonValue(item); err != nil {
			return
		}
		switch k := key.(type) {
		case lua.LString:
			object[string(k)] = converted
		case lua.LNumber:
			array = append(array, converted)
		default:
			err = fmt.Errorf("cannot encode a table with a %s key to JSON", key.Type())
		}
	})
	switch {
	case err != nil:
		return nil, err
	case len(object) > 0 && len(array) > 0:
		return nil, errors.New("cannot encode a table with both string and number keys to JSON")
	case len(object) > 0:
		return object, nil
	case len(array) != table.Len():
		return nil, errors.New("cannot encode a sparse array to JSON")
	default:
		return array, nil
	}
}

// luaValue converts a value of unstructured content to Lua, as Argo CD's util/lua decodeValue does: numbers become
// Lua numbers, a json.Number a string, slices tables with indices from 1, maps tables, and anything else nil.
func luaValue(l *lua.LState, value any) lua.LValue {
	switch v := value.(type) {
	case bool:
		return lua.LBool(v)
	case float64:
		return lua.LNumber(v)
	case string:
		return lua.LString(v)
	case json.Number:
		return lua.LString(v)
	case int:
		return lua.LNumber(v)
	case int32:
		return lua.LNumber(v)
	case int64:
		return lua.LNumber(v)
	case []any:
		table := l.CreateTable(len(v), 0)
		for _, item := range v {
			table.Append(luaValue(l, item))
		}
		return table
	case map[string]any:
		table := l.CreateTable(0, len(v))
		for key, item := range v {
			table.RawSetH(lua.LString(key), luaValue(l, item))
		}
		return table
	default:
		return lua.LNil
	}
}
