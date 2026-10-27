/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"go/types"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// The controller of a kind whose settings go by their IDs (settingsbyid.go):
// its settingsKind reads, writes and resets each setting with one call of its
// service, through the shared helpers of settings_byid.go, and its conversions
// turn the spec into the writes it asks for, each setting's value as the
// write's union of values takes it, and into the writes that reset them.

// emitSettingsByIDDescriptor writes the settingsKind of a kind whose settings
// go by their IDs.
func emitSettingsByIDDescriptor(f *goFile, k *settingsKind) error {
	name, v, svc := k.name, k.varName, k.svc
	api := name + "API"
	ctx := f.use("context", "context")
	obj := "*" + f.api() + "." + name
	writes, resets := k.conversions().name(k.params), v+"Resets"
	var doc strings.Builder
	fmt.Fprintf(&doc, "%sKind describes %s to the shared settings reconciler. %s configures the %s of its zone, "+
		"each by its ID: it writes the settings its spec sets, and resets them when it is deleted. A zone nobody has "+
		"configured holds %s.", v, pluralOf(name), upperCamel(article(name)), k.noun,
		prose(unconfiguredWords(k.unconfigured)))
	for _, id := range slices.Sorted(maps.Keys(k.picks)) {
		pick := k.picks[id]
		fmt.Fprintf(&doc, " Cloudflare picks %s itself while %s is %q.", id, pick.Setting, pick.While)
	}
	comment(&f.Builder, "", wrap(doc.String()))
	emitSettingsKindHead(f, k)
	f.printf("byID: true,\n\n")

	f.printf("read: func(ctx %s.Context, api %s, zoneID string, obj settingsObject) (string, error) {\n", ctx, api)
	f.printf("spec := obj.(%s).Spec\n", obj)
	f.printf("return readSettingsByID(ctx, settingIDs(%s(spec), %s(spec)), func(ctx %s.Context, id string) (string, "+
		"error) {\n", writes, resets, ctx)
	if err := zoneParams(f, svc.read); err != nil {
		return err
	}
	f.printf("setting, err := api.%s(ctx, id, params)\nif err != nil {\nreturn \"\", err\n}\n", svc.read.name)
	f.printf("return setting.JSON.RawJSON(), nil\n})\n},\n")
	f.printf("write: func(ctx %s.Context, api %s, zoneID string, obj settingsObject, ids []string) error {\n", ctx,
		api)
	f.printf("return writeSettingsByID(ctx, settingsNamed(%s(obj.(%s).Spec), ids), %sEdit(api, zoneID))\n},\n",
		writes, obj, v)
	f.printf("reset: func(ctx %s.Context, api %s, zoneID string, obj settingsObject) error {\n", ctx, api)
	f.printf("return writeSettingsByID(ctx, %s(obj.(%s).Spec), %sEdit(api, zoneID))\n},\n", resets, obj, v)
	f.printf("desired: func(obj settingsObject) ([]byte, error) {\n")
	f.printf("return settingsByIDJSON(%s(obj.(%s).Spec), %sBody)\n},\n\n", writes, obj, v)
	emitSettingsKindTail(f, k)
	return nil
}

// emitSettingsByIDConversions writes the conversions of a kind whose settings
// go by their IDs: the type of a write of one setting, the edit and the body
// that make one, the writes the spec asks for and those that reset them. The
// conversions of the settings' objects follow, as emitConversion writes them.
func emitSettingsByIDConversions(f *goFile, k *settingsKind) error {
	name, v, svc := k.name, k.varName, k.svc
	api := name + "API"
	ctx := f.use("context", "context")
	b := svc.writeByIDBody
	union, body := f.typ(b.value.typ), f.typ(b.body)
	write := v + "Write"

	comment(&f.Builder, "", wrap(fmt.Sprintf("%s is a write of one setting of %s: the setting's ID, and its value as "+
		"cloudflare-go v7's %s takes it.", write, article(name), svc.write.name)))
	f.printf("type %s = settingValue[%s]\n\n", write, union)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sEdit returns the write of one setting of the zone zoneID, with "+
		"cloudflare-go v7's %s.", v, svc.write.name)))
	f.printf("func %sEdit(api %s, zoneID string) func(%s.Context, string, %s) error {\n", v, api, ctx, union)
	f.printf("return func(ctx %s.Context, id string, value %s) error {\n", ctx, union)
	if err := zoneParams(f, svc.write); err != nil {
		return err
	}
	f.printf("params.%s = %sBody(value)\n", b.field.goName, v)
	f.printf("_, err := api.%s(ctx, id, params)\nreturn err\n}\n}\n\n", svc.write.name)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sBody returns the body with which cloudflare-go v7's %s writes a "+
		"setting's value.", v, svc.write.name)))
	f.printf("func %sBody(value %s) %s {\nvar body %s\n", v, union, body, body)
	f.printf("body.%s.Value, body.%s.Present = value, true\nreturn body\n}\n\n", b.value.goName, b.value.goName)

	c := k.conversions()
	comment(&f.Builder, "", wrap(fmt.Sprintf("%s returns %s.", c.root, c.rootWhat)))
	f.printf("func %s(in %s.%sSpec) []%s {\nvar out []%s\n", c.root, f.api(), name, write, write)
	for _, pf := range k.params.fields {
		condition, value, err := settingValueExpr(f, c, pf)
		if err != nil {
			return fmt.Errorf("%s: %w", pf.apiName, err)
		}
		f.printf("if %s {\nout = append(out, %s{id: %q, value: %s})\n}\n", condition, write, pf.apiName, value)
	}
	f.printf("return out\n}\n\n")

	var doc strings.Builder
	fmt.Fprintf(&doc, "%sResets returns the writes that reset the settings %s asks for, each to the value a zone "+
		"nobody has configured holds", v, article(name))
	for _, id := range slices.Sorted(maps.Keys(k.picks)) {
		pick := k.picks[id]
		fmt.Fprintf(&doc, "; %s through %s %q, which hands it back to Cloudflare", id, pick.Setting, pick.While)
	}
	comment(&f.Builder, "", wrap(doc.String()+"."))
	f.printf("func %sResets(in %s.%sSpec) []%s {\nvar out []%s\n", v, f.api(), name, write, write)
	stringVariant, err := svc.valueVariant(types.Typ[types.String])
	if err != nil && len(k.picks) > 0 {
		return fmt.Errorf("the settings it is picked by: %w", err)
	}
	for _, pf := range k.params.fields {
		condition, _, err := settingValueExpr(f, c, pf)
		if err != nil {
			return fmt.Errorf("%s: %w", pf.apiName, err)
		}
		id, value := pf.apiName, ""
		if pick, picked := k.picks[pf.apiName]; picked {
			id, value = pick.Setting, f.typ(stringVariant)+"("+strconv.Quote(pick.While)+")"
		} else if value, err = resetExpr(f, c, pf, k.resetValue(pf)); err != nil {
			return fmt.Errorf("%s: %w", pf.apiName, err)
		}
		f.printf("if %s {\nout = append(out, %s{id: %q, value: %s})\n}\n", condition, write, id, value)
	}
	f.printf("return out\n}\n\n")
	return nil
}

// settingValueExpr returns, for a setting's field of a kind whose settings go
// by their IDs, the condition under which the spec sets it, and its value as
// the write's union of values takes it.
func settingValueExpr(f *goFile, c conversions, pf *paramField) (condition, value string, err error) {
	in := "in." + pf.goName
	switch n := pf.node.(type) {
	case *paramScalar:
		condition = in + " != " + zeroOf(n.goType)
		if pf.pointer() {
			condition, in = in+" != nil", "*"+in
		}
		value, err = scalarExpr(f, n, in, pf.sdkType)
		return condition, value, err
	case *paramStruct:
		if !pf.pointer() {
			return "", "", fmt.Errorf("a setting's object is a %s, not a pointer", pf.goType())
		}
		return in + " != nil", c.name(n) + "(*" + in + ")", nil
	}
	return "", "", fmt.Errorf("kflare-gen cannot write a list by the setting's ID yet")
}

// resetExpr returns the value a reset writes to a setting of a kind whose
// settings go by their IDs, as the write's union of values takes it, from the
// value as JSON decodes it.
func resetExpr(f *goFile, c conversions, pf *paramField, value any) (string, error) {
	literal, err := goLiteral(f, pf.node, value, false)
	if err != nil {
		return "", err
	}
	switch n := pf.node.(type) {
	case *paramScalar:
		return scalarExpr(f, n, literal, pf.sdkType)
	case *paramStruct:
		return c.name(n) + "(" + literal + ")", nil
	}
	return "", fmt.Errorf("kflare-gen cannot reset a list by the setting's ID yet")
}

// goLiteral writes a value of a param node, as JSON decodes it, as a Go
// literal of the API types: a pointer to it when pointer is set.
func goLiteral(f *goFile, node paramNode, value any, pointer bool) (string, error) {
	var literal string
	switch n := node.(type) {
	case *paramScalar:
		switch n.goType {
		case typeString:
			s, ok := value.(string)
			if !ok {
				return "", fmt.Errorf("%v is not a string", value)
			}
			literal = strconv.Quote(s)
		case typeBool:
			on, ok := value.(bool)
			if !ok {
				return "", fmt.Errorf("%v is not a bool", value)
			}
			literal = strconv.FormatBool(on)
		case typeInt64:
			i, ok := wholeNumber(value)
			if !ok {
				return "", fmt.Errorf("%v is not a whole number", value)
			}
			literal = strconv.FormatInt(i, 10)
			if pointer {
				literal = "int64(" + literal + ")"
			}
		}
		if pointer {
			return "new(" + literal + ")", nil
		}
		return literal, nil
	case *paramStruct:
		object, ok := value.(map[string]any)
		if !ok {
			return "", fmt.Errorf("%v is not an object", value)
		}
		var fields []string
		for _, pf := range n.fields {
			v, ok := object[pf.apiName]
			if !ok {
				continue
			}
			literal, err := goLiteral(f, pf.node, v, pf.pointer())
			if err != nil {
				return "", fmt.Errorf("%s: %w", pf.apiName, err)
			}
			fields = append(fields, pf.goName+": "+literal)
		}
		literal = f.api() + "." + n.goName + "{" + strings.Join(fields, ", ") + "}"
		if pointer {
			literal = "&" + literal
		}
		return literal, nil
	}
	return "", fmt.Errorf("kflare-gen cannot write a list's value as Go yet")
}
