/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package main

import (
	"fmt"
	"strings"
)

// hookedKind is the model of a hooked kind: its API types, and its reconciler
// on the shared CRUD one. The rest of it is hand-written (hookedKindConfig).
type hookedKind struct {
	*kind
	varName string
	// usesAccount is set for a kind whose spec holds an accountRef.
	usesAccount bool
}

// buildHookedKind builds the model of entry's kind.
func buildHookedKind(entry hookedKindConfig) *hookedKind {
	k := &kind{name: entry.Kind, shortName: entry.ShortName, noun: entry.Noun, plural: plural(entry.Kind),
		doc: lines(entry.Doc), claimLabel: entry.ClaimLabel, fixed: map[string]string{},
		specExtras: entry.SpecExtras, metadataExtras: entry.MetadataExtras, statusExtras: entry.StatusExtras}
	if entry.Scope == scopeAccount {
		k.spec = append(k.spec, accountRefSpecField(entry.Noun))
	}
	k.metadata = append(k.metadata, field{
		apiName: entry.ID.Field, goName: upperCamel(entry.ID.Status), jsonName: entry.ID.Status, goType: typeString,
		doc: lines(entry.ID.Doc), optional: true,
	})
	for _, c := range entry.Columns {
		k.printColumns = append(k.printColumns, printColumn{name: c.Name, typ: c.Type, jsonPath: c.Path})
	}
	k.printColumns = append(k.printColumns, readyColumn, ageColumn)
	return &hookedKind{kind: k, varName: lowerCamel(entry.Kind), usesAccount: entry.Scope == scopeAccount}
}

// emitHookedController returns the Go source of a hooked kind's reconciler.
func emitHookedController(k *hookedKind, header string) ([]byte, error) {
	f := newGoFile()
	name, v := k.name, k.varName
	api := name + "API"
	obj := "*" + f.api() + "." + name
	ctx := f.use("context", "context")
	ctrl := f.use("sigs.k8s.io/controller-runtime", "ctrl")
	crud := fmt.Sprintf("crudResourceReconciler[%s, %s, %sObject, %sListed]", obj, api, v, v)

	comment(&f.Builder, "", wrap(fmt.Sprintf("%sReconciler reconciles %s object, with the shared CRUD reconciler "+
		"(crudresource_controller.go).", name, article(name))))
	f.printf("type %sReconciler struct {\n\t%s.Client\n\tScheme *%s.Scheme\n\n", name, f.use(clientPackage, "client"),
		f.use("k8s.io/apimachinery/pkg/runtime", "runtime"))
	comment(&f.Builder, "\t", wrapIndented(fmt.Sprintf("New%s constructs %s from a raw API token. Defaults to "+
		"default%s; tests inject a fake.", api, article(api), api)))
	f.printf("\tNew%s func(token string) (%s, error)\n}\n\n", api, api)

	group := "+kubebuilder:rbac:groups=kflare.dev,resources="
	for _, marker := range []string{
		group + k.plural + ",verbs=get;list;watch;create;update;patch;delete",
		group + k.plural + "/status,verbs=get;update;patch",
		group + k.plural + "/finalizers,verbs=update",
		group + "cloudflareaccounts,verbs=get;list;watch",
		`+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch`,
	} {
		f.printf("// %s\n", marker)
	}
	f.printf("\n")

	f.printf("func (r *%sReconciler) Reconcile(ctx %s.Context, req %s.Request) (%s.Result, error) {\n", name, ctx, ctrl,
		ctrl)
	f.printf("return r.crud().reconcile(ctx, req)\n}\n\n")
	comment(&f.Builder, "", wrap(fmt.Sprintf("SetupWithManager registers %sReconciler with the manager, with the "+
		"shared CRUD reconciler's watches and those of the kind's hooks.", name)))
	f.printf("func (r *%sReconciler) SetupWithManager(mgr %s.Manager) error {\nreturn r.crud().setup(mgr, r)\n}\n\n", name,
		ctrl)
	comment(&f.Builder, "", wrap(fmt.Sprintf("crud returns the shared CRUD reconciler for %s, with the injected "+
		"factory or the production one.", pluralOf(name))))
	f.printf("func (r *%sReconciler) crud() *%s {\n", name, crud)
	f.printf("newAPI := r.New%s\nif newAPI == nil {\nnewAPI = default%s\n}\n", api, api)
	f.printf("return &%s{\nClient: r.Client, kind: %sKind, newAPI: newAPI,\n}\n}\n", crud, v)
	return f.source(header)
}

// addHookedKind adds a hooked kind's files. Its sample stays hand-written, as
// its fields are.
func (o *outputs) addHookedKind(k *hookedKind, header string) error {
	lower := strings.ToLower(k.name)
	types, err := emitTypes(k.kind, header)
	if err != nil {
		return err
	}
	ctrl, err := emitHookedController(k, header)
	if err != nil {
		return err
	}
	o.files[typesFile(lower)] = types
	o.files[controllerFile(lower)] = ctrl
	return nil
}
