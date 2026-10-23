/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var _ = Describe("settingsShape.matches", func() {
	shape := settingsShape{
		listKeys: map[string]string{"items": "id", "outer[].inner": "name"},
		sets:     map[string]bool{"tags": true, "nested.tags": true},
	}

	DescribeTable("compares only what the desired settings hold",
		func(desired, actual string, want bool) {
			got, err := shape.matches([]byte(desired), []byte(actual))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("equal scalars", `{"a":"x","b":1,"c":true}`, `{"a":"x","b":1,"c":true}`, true),
		Entry("a different string", `{"a":"x"}`, `{"a":"y"}`, false),
		Entry("a number for a string", `{"a":"1"}`, `{"a":1}`, false),
		Entry("a different bool", `{"c":false}`, `{"c":true}`, false),
		Entry("what Cloudflare adds", `{"a":"x"}`, `{"a":"x","computed":"z","conflicts":["y"]}`, true),
		Entry("a field Cloudflare does not hold", `{"a":"x"}`, `{}`, false),
		Entry("a null asked for, as left out", `{"a":null}`, `{}`, true),
		Entry("an empty object and list asked for, as left out", `{"o":{},"l":[]}`, `{}`, true),
		Entry("an empty value Cloudflare holds, as left out", `{"a":"x"}`, `{"a":"x","o":{},"l":[],"n":null}`,
			true),
		Entry("an object for a scalar", `{"o":{"a":"x"}}`, `{"o":"x"}`, false),
		Entry("nested objects", `{"o":{"a":"x"}}`, `{"o":{"a":"x","b":"y"}}`, true),
		Entry("a nested difference", `{"o":{"a":"x"}}`, `{"o":{"a":"y"}}`, false),

		Entry("keyed items, in another order, among others", `{"items":[{"id":"b","on":true},{"id":"a","on":false}]}`,
			`{"items":[{"id":"a","on":false,"x":1},{"id":"c","on":true},{"id":"b","on":true}]}`, true),
		Entry("a keyed item Cloudflare does not hold", `{"items":[{"id":"d","on":true}]}`,
			`{"items":[{"id":"a","on":true}]}`, false),
		Entry("a keyed item that differs", `{"items":[{"id":"a","on":true}]}`, `{"items":[{"id":"a","on":false}]}`,
			false),
		Entry("a keyed item without its key", `{"items":[{"on":true}]}`, `{"items":[{"id":"a","on":true}]}`, false),
		Entry("keyed items both without their key", `{"items":[{"on":true}]}`, `{"items":[{"on":true}]}`, false),
		Entry("keyed items that are not objects", `{"items":["a"]}`, `{"items":["a"]}`, false),
		Entry("keyed items Cloudflare holds as no objects", `{"items":[{"id":"a"}]}`, `{"items":["a"]}`, false),
		Entry("a list for a keyed list's object", `{"items":[{"id":"a"}]}`, `{"items":{"id":"a"}}`, false),
		Entry("keyed items nested in a list", `{"outer":[{"inner":[{"name":"n","v":1}]}]}`,
			`{"outer":[{"inner":[{"name":"m","v":2},{"name":"n","v":1}]}]}`, true),

		Entry("a set in another order", `{"tags":["a","b"]}`, `{"tags":["b","a"]}`, true),
		Entry("a set with another item", `{"tags":["a","b"]}`, `{"tags":["a","c"]}`, false),
		Entry("a set with more items", `{"tags":["a"]}`, `{"tags":["a","b"]}`, false),
		Entry("a set counted, not only listed", `{"tags":["a","a"]}`, `{"tags":["a","b"]}`, false),
		Entry("a nested set in another order", `{"nested":{"tags":["b","a"]}}`, `{"nested":{"tags":["a","b"]}}`,
			true),

		Entry("a list in order", `{"list":["a","b"]}`, `{"list":["a","b"]}`, true),
		Entry("a list in another order", `{"list":["a","b"]}`, `{"list":["b","a"]}`, false),
		Entry("a list of another length", `{"list":["a"]}`, `{"list":["a","b"]}`, false),
		Entry("a list's objects with what Cloudflare adds", `{"list":[{"a":1}]}`, `{"list":[{"a":1,"b":2}]}`, true),
		Entry("a list's object that differs", `{"list":[{"a":1}]}`, `{"list":[{"a":2}]}`, false),
		Entry("a list for a scalar", `{"list":["a"]}`, `{"list":"a"}`, false),
	)

	It("refuses what is not JSON", func() {
		_, err := shape.matches([]byte(`{`), []byte(`{}`))
		Expect(err).To(HaveOccurred())
		_, err = shape.matches([]byte(`{}`), []byte(`nope`))
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("settingsConfigured", func() {
	unconfigured := []settingsValue{
		{path: "scope", value: "incoming"},
		{path: "items[].enabled", value: false},
		{path: "nested.level", value: float64(1)},
	}

	DescribeTable("tells settings configured outside kflare from a zone nobody configured",
		func(settings string, want bool) {
			got, err := settingsConfigured([]byte(settings), unconfigured)
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("every value an unconfigured zone holds", `{"scope":"incoming","items":[{"enabled":false},{"enabled":false}],
			"nested":{"level":1}}`, false),
		Entry("another scope", `{"scope":"both"}`, true),
		Entry("one item enabled", `{"items":[{"enabled":false},{"enabled":true}]}`, true),
		Entry("another nested level", `{"nested":{"level":2}}`, true),
		Entry("nothing at the paths", `{"other":"x"}`, false),
		Entry("no items", `{"items":[]}`, false),
		Entry("items that are not objects", `{"items":["x"]}`, false),
		Entry("a scalar where an object goes", `{"nested":"x"}`, false),
		Entry("a list that is not one", `{"items":{"enabled":true}}`, false),
		Entry("a null where a value goes", `{"scope":null,"items":[{"enabled":null}]}`, false),
	)

	It("refuses what is not JSON", func() {
		_, err := settingsConfigured([]byte(`[`), unconfigured)
		Expect(err).To(HaveOccurred())
	})
})

var _ = Describe("settingsPrecede", func() {
	recording := func(name, zoneID string) *SettingsFixture {
		obj := &SettingsFixture{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}}
		obj.Status.CloudflareMetadata.ZoneID = zoneID
		return obj
	}

	It("puts the resource that records the zone first, then the older one", func() {
		Expect(settingsPrecede(recording("b", "zone"), recording("a", ""), "zone")).To(BeTrue())
		Expect(settingsPrecede(recording("a", ""), recording("b", "zone"), "zone")).To(BeFalse())
		Expect(settingsPrecede(recording("b", "other"), recording("a", ""), "zone")).To(BeFalse())
		Expect(settingsPrecede(recording("a", "zone"), recording("b", "zone"), "zone")).To(BeTrue())
		Expect(settingsPrecede(recording("b", "zone"), recording("a", "zone"), "zone")).To(BeFalse())
	})
})

var _ = Describe("settingsOlder", func() {
	at := func(namespace, name string, created time.Time) *SettingsFixture {
		return &SettingsFixture{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name,
			CreationTimestamp: metav1.NewTime(created)}}
	}
	now := time.Now().Truncate(time.Second)

	It("orders by creation time, then namespace, then name", func() {
		Expect(settingsOlder(at("b", "b", now), at("a", "a", now.Add(time.Second)))).To(BeTrue())
		Expect(settingsOlder(at("a", "a", now.Add(time.Second)), at("b", "b", now))).To(BeFalse())
		Expect(settingsOlder(at("a", "z", now), at("b", "a", now))).To(BeTrue())
		Expect(settingsOlder(at("b", "a", now), at("a", "z", now))).To(BeFalse())
		Expect(settingsOlder(at("a", "a", now), at("a", "b", now))).To(BeTrue())
		Expect(settingsOlder(at("a", "b", now), at("a", "a", now))).To(BeFalse())
		Expect(settingsOlder(at("a", "a", now), at("a", "a", now))).To(BeFalse())
	})
})
