/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"

	"github.com/cloudflare/cloudflare-go/v7/dns"
	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudflarev1alpha1 "github.com/JonathanGraniero/kflare/api/v1alpha1"
)

// automaticTTL is the TTL Cloudflare reports for "automatic". It is also what
// Cloudflare stores when a record is created without a TTL.
const automaticTTL = 1

// tunnelRefField is the spec field of a DNSRecord's reference to a Tunnel.
const tunnelRefField = "tunnelRef"

// dnsRecordHooks are what a DNSRecord does beyond the shared loop. Several
// records of a set share a name and a type, so a resource tells its own by its
// content; a record can point at a Tunnel; and Cloudflare fills in what a spec
// leaves unset, so the comparison has rules of its own.
var dnsRecordHooks = crudHooks[*cloudflarev1alpha1.DNSRecord, DNSRecordAPI, dns.RecordResponse]{
	references: []crudReference[*cloudflarev1alpha1.DNSRecord]{
		reference(tunnelRefField, "Tunnel", dnsRecordTunnelName,
			func(ref *cloudflarev1alpha1.Tunnel) string { return ref.Spec.AccountRef.Name },
			func(ref *cloudflarev1alpha1.Tunnel) string { return ref.Status.CloudflareMetadata.TunnelID }),
	},
	find:    findDNSRecord,
	matches: dnsRecordMatches,
}

// dnsRecordTunnelName returns the Tunnel a DNSRecord's tunnelRef names: "" when
// it has none.
func dnsRecordTunnelName(record *cloudflarev1alpha1.DNSRecord) string {
	if record.Spec.TunnelRef == nil {
		return ""
	}
	return record.Spec.TunnelRef.Name
}

// dnsRecordContent returns the content kflare writes: the hostname of the
// tunnel a tunnelRef gives, or else the spec's.
func dnsRecordContent(record *cloudflarev1alpha1.DNSRecord, refs map[string]string) string {
	if id := refs[tunnelRefField]; id != "" {
		return id + cloudflarev1alpha1.TunnelHostnameSuffix
	}
	return record.Spec.Content
}

// dnsRecordProxied returns whether a record is proxied: always with a
// tunnelRef, since an unproxied one resolves to an address the internet cannot
// reach (verified live), and otherwise as the spec says, unset meaning false.
func dnsRecordProxied(record *cloudflarev1alpha1.DNSRecord, refs map[string]string) bool {
	return refs[tunnelRefField] != "" || record.Spec.Proxied != nil && *record.Spec.Proxied
}

// dnsRecordTTL returns spec.ttl, or automatic when it is unset.
func dnsRecordTTL(record *cloudflarev1alpha1.DNSRecord) dns.TTL {
	if record.Spec.TTL == 0 {
		return automaticTTL
	}
	return dns.TTL(record.Spec.TTL)
}

// dnsRecordData returns spec.data as the value cloudflare-go sends: nil when it
// is unset. The API server has checked that it is a JSON object.
func dnsRecordData(record *cloudflarev1alpha1.DNSRecord) any {
	if record.Spec.Data == nil {
		return nil
	}
	var data any
	_ = json.Unmarshal(record.Spec.Data.Raw, &data)
	return data
}

// dnsRecordNewBody finishes the body of a new record with what cloudflare-go
// v7 does not type, and what a tunnelRef implies.
func dnsRecordNewBody(record *cloudflarev1alpha1.DNSRecord, refs map[string]string, body *dns.RecordNewParamsBody) {
	body.TTL.Value, body.TTL.Present = dnsRecordTTL(record), true
	if content := dnsRecordContent(record, refs); content != "" {
		body.Content.Value, body.Content.Present = content, true
	}
	if record.Spec.Proxied != nil || refs[tunnelRefField] != "" {
		body.Proxied.Value, body.Proxied.Present = dnsRecordProxied(record, refs), true
	}
	if record.Spec.Priority != nil {
		body.Priority.Value, body.Priority.Present = float64(*record.Spec.Priority), true
	}
	if len(record.Spec.Tags) > 0 {
		body.Tags.Value, body.Tags.Present = record.Spec.Tags, true
	}
	if data := dnsRecordData(record); data != nil {
		body.Data.Value, body.Data.Present = data, true
	}
}

// dnsRecordEditBody finishes the body of a change: the record's name and type,
// which Edit requires and the entry keeps immutable, and the values Cloudflare
// must not keep. Tags and proxying are always sent, so that unsetting them
// clears them. A priority the spec leaves unset is not sent, so Cloudflare's
// stays.
func dnsRecordEditBody(record *cloudflarev1alpha1.DNSRecord, refs map[string]string, body *dns.RecordEditParamsBody) {
	body.Name.Value, body.Name.Present = record.Spec.Name, true
	body.Type.Value, body.Type.Present = dns.RecordEditParamsBodyType(record.Spec.Type), true
	body.TTL.Value, body.TTL.Present = dnsRecordTTL(record), true
	if content := dnsRecordContent(record, refs); content != "" {
		body.Content.Value, body.Content.Present = content, true
	}
	body.Proxied.Value, body.Proxied.Present = dnsRecordProxied(record, refs), true
	if record.Spec.Priority != nil {
		body.Priority.Value, body.Priority.Present = float64(*record.Spec.Priority), true
	}
	tags := record.Spec.Tags
	if tags == nil {
		tags = []string{}
	}
	body.Tags.Value, body.Tags.Present = tags, true
	if data := dnsRecordData(record); data != nil {
		body.Data.Value, body.Data.Present = data, true
	}
}

// storedDNSRecord is what a record holds that cloudflare-go v7 types by the
// record's type: kflare reads it as Cloudflare sent it.
type storedDNSRecord struct {
	Data any      `json:"data"`
	Tags []string `json:"tags"`
}

// readDNSRecord returns the data and tags of current.
func readDNSRecord(current *dns.RecordResponse) storedDNSRecord {
	var stored storedDNSRecord
	// A response cloudflare-go decoded is valid JSON; an empty one holds neither.
	_ = json.Unmarshal([]byte(current.JSON.RawJSON()), &stored)
	return stored
}

// dnsRecordMatches compares a record with its spec. Unset fields are compared
// against Cloudflare's defaults, because Cloudflare always reports a value for
// them: an unset ttl means automatic and an unset proxied means false. An unset
// priority is left to Cloudflare. Content is not compared when spec.data is
// set, or for SRV records, because Cloudflare derives it from the data fields.
func dnsRecordMatches(record *cloudflarev1alpha1.DNSRecord, current *dns.RecordResponse, refs map[string]string) bool {
	spec, stored := &record.Spec, readDNSRecord(current)
	contentFromData := spec.Data != nil || spec.Type == "SRV"
	return (contentFromData || dnsRecordContent(record, refs) == current.Content) &&
		dnsRecordTTL(record) == current.TTL &&
		dnsRecordProxied(record, refs) == current.Proxied &&
		(spec.Priority == nil || float64(*spec.Priority) == current.Priority) &&
		spec.Comment == current.Comment &&
		tagsEqual(spec.Tags, stored.Tags) &&
		reflect.DeepEqual(dnsRecordData(record), stored.Data)
}

// tagsEqual returns true if both tag slices contain the same elements (order-independent).
func tagsEqual(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// findDNSRecord returns the ID of the record that is record's, among the
// zone's records of its name and type, or "" when a new record joins them. It is
// the one its claim names, or the one that holds what the spec asks for, so
// DNSRecords for the members of a set (several MX, TXT or A records) each find
// their own. That one may be another DNSRecord's: Cloudflare refuses two
// identical records, so the shared loop reports the conflict. With an adoption
// policy, a record that does not match is it when it is the only one and no
// other DNSRecord manages it: without one, an existing record whose content
// differs is left as it is, a separate member of the set.
func findDNSRecord(ctx context.Context, c client.Client, api DNSRecordAPI, record *cloudflarev1alpha1.DNSRecord,
	zoneID string, refs map[string]string) (string, error) {
	candidates, err := listDNSRecordSet(ctx, api, record, zoneID)
	if err != nil {
		return "", err
	}
	for i := range candidates {
		if ownsID(record, cloudflarev1alpha1.DNSRecordIDLabel, candidates[i].ID) {
			return candidates[i].ID, nil
		}
	}
	for i := range candidates {
		if dnsRecordHolds(record, &candidates[i], refs) {
			return candidates[i].ID, nil
		}
	}
	// An unknown policy is reported by the shared loop, by mayAdopt or mayCreate.
	if policy, _ := adoptionPolicy(record); policy == "" || len(candidates) != 1 {
		return "", nil
	}
	claimed, err := claimedIDs(ctx, c, &cloudflarev1alpha1.DNSRecordList{}, cloudflarev1alpha1.DNSRecordIDLabel, record)
	if err != nil {
		return "", err
	}
	if _, taken := claimed[candidates[0].ID]; taken {
		return "", nil
	}
	return candidates[0].ID, nil
}

// listDNSRecordSet returns the zone's records with record's name and type.
func listDNSRecordSet(ctx context.Context, api DNSRecordAPI, record *cloudflarev1alpha1.DNSRecord,
	zoneID string) ([]dns.RecordResponse, error) {
	var set []dns.RecordResponse
	for page := int64(1); ; page++ {
		params := dns.RecordListParams{}
		params.ZoneID.Value, params.ZoneID.Present = zoneID, true
		params.Name.Value.Exact.Value, params.Name.Value.Exact.Present = record.Spec.Name, true
		params.Name.Present = true
		params.Type.Value, params.Type.Present = dns.RecordListParamsType(record.Spec.Type), true
		params.Page.Value, params.Page.Present = float64(page), true
		params.PerPage.Value, params.PerPage.Present = dnsRecordListPageSize, true
		res, err := api.List(ctx, params)
		if err != nil {
			return nil, err
		}
		for i := range res.Result {
			// Cloudflare's name filter ignores case.
			if strings.EqualFold(res.Result[i].Name, record.Spec.Name) && string(res.Result[i].Type) == record.Spec.Type {
				set = append(set, res.Result[i])
			}
		}
		if len(res.Result) == 0 || int64(len(res.Result)) < res.ResultInfo.PerPage {
			return set, nil
		}
	}
}

// dnsRecordHolds reports whether current holds the value record describes: its
// data when data is set, otherwise its content.
func dnsRecordHolds(record *cloudflarev1alpha1.DNSRecord, current *dns.RecordResponse, refs map[string]string) bool {
	if record.Spec.Data != nil {
		return reflect.DeepEqual(dnsRecordData(record), readDNSRecord(current).Data)
	}
	return dnsRecordContent(record, refs) == current.Content
}

// isZoneReady returns true if the Zone has a Ready=True condition and a zone ID.
func isZoneReady(zone *cloudflarev1alpha1.Zone) bool {
	return zone.Status.CloudflareMetadata.ZoneID != "" &&
		meta.IsStatusConditionTrue(zone.Status.Conditions, cloudflarev1alpha1.ConditionReady)
}
