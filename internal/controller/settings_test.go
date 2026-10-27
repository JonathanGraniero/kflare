/*
Copyright 2026 Jonathan Graniero.

SPDX-License-Identifier: MIT
*/

package controller

import (
	"context"
	"errors"
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

var _ = Describe("settingsShape.differing", func() {
	shape := settingsShape{listKeys: map[string]string{"items": "id"}}

	DescribeTable("names the settings, by their IDs, that Cloudflare does not hold as asked for",
		func(desired, actual string, want []string) {
			got, err := shape.differing([]byte(desired), []byte(actual))
			Expect(err).NotTo(HaveOccurred())
			Expect(got).To(Equal(want))
		},
		Entry("none, with what Cloudflare adds", `{"a":"on","o":{"x":1}}`, `{"a":"on","o":{"x":1,"y":2},"b":"off"}`,
			[]string(nil)),
		Entry("the one that differs", `{"a":"on","b":"off"}`, `{"a":"on","b":"on"}`, []string{"b"}),
		Entry("each, in the order of their IDs", `{"z":1,"a":1,"m":1}`, `{}`, []string{"a", "m", "z"}),
		Entry("an object with a field that differs", `{"o":{"x":1,"y":2}}`, `{"o":{"x":1,"y":3}}`, []string{"o"}),
		Entry("a keyed list by its keys", `{"items":[{"id":"b"},{"id":"a"}]}`, `{"items":[{"id":"a"},{"id":"b"}]}`,
			[]string(nil)),
		Entry("nulls and empty objects asked for, as left out", `{"a":null,"o":{}}`, `{}`, []string(nil)),
		Entry("a null and an empty list asked for, as left out, whatever Cloudflare holds", `{"a":null,"l":[]}`,
			`{"a":"x","l":["y"]}`, []string(nil)),
	)

	It("refuses what is not JSON", func() {
		_, err := shape.differing([]byte(`{`), []byte(`{}`))
		Expect(err).To(HaveOccurred())
		_, err = shape.differing([]byte(`{}`), []byte(`[]`))
		Expect(err).To(HaveOccurred())
	})
})

// settingsTestBody is a body of a write of one setting, as the tests of the
// settings by ID give one: data is its JSON, or err the error marshaling it
// fails with.
type settingsTestBody struct {
	data string
	err  error
}

func (b settingsTestBody) MarshalJSON() ([]byte, error) { return []byte(b.data), b.err }

var _ = Describe("settings by ID", func() {
	ctx := context.Background()
	settings := []settingValue[string]{{id: "b", value: "2"}, {id: "a", value: "1"}, {id: "c", value: "3"}}

	It("lists the settings' IDs once each, in order", func() {
		Expect(settingIDs(settings, []settingValue[string]{{id: "a"}, {id: "d"}})).To(Equal([]string{"a", "b", "c", "d"}))
		Expect(settingIDs[string]()).To(BeEmpty())
	})

	It("names the settings a sync writes, in the order of the settings", func() {
		Expect(settingsNamed(settings, []string{"c", "b"})).To(Equal([]settingValue[string]{
			{id: "b", value: "2"}, {id: "c", value: "3"}}))
		Expect(settingsNamed(settings, nil)).To(BeEmpty())
	})

	It("reads each setting's value under its ID, a missing value as null", func() {
		var read []string
		got, err := readSettingsByID(ctx, []string{"a", "b"}, func(_ context.Context, id string) (string, error) {
			read = append(read, id)
			if id == "b" {
				return `{"id":"b","editable":false}`, nil
			}
			return `{"id":"a","value":{"x":[1,2]},"editable":true}`, nil
		})
		Expect(err).NotTo(HaveOccurred())
		Expect(read).To(Equal([]string{"a", "b"}))
		Expect(got).To(MatchJSON(`{"a":{"x":[1,2]},"b":null}`))
	})

	It("stops reading at the first error, and refuses a setting that is not JSON", func() {
		var read []string
		_, err := readSettingsByID(ctx, []string{"a", "b"}, func(_ context.Context, id string) (string, error) {
			read = append(read, id)
			return "", errors.New("refused")
		})
		Expect(err).To(MatchError("refused"))
		Expect(read).To(Equal([]string{"a"}))

		_, err = readSettingsByID(ctx, []string{"a"}, func(context.Context, string) (string, error) {
			return `{"value":`, nil
		})
		Expect(err).To(MatchError(ContainSubstring("reading the setting a")))
	})

	It("writes each setting in order, stopping at the first error", func() {
		var written []string
		edit := func(_ context.Context, id, value string) error {
			written = append(written, id+"="+value)
			if id == "a" {
				return errors.New("refused")
			}
			return nil
		}
		Expect(writeSettingsByID(ctx, settings[:1], edit)).To(Succeed())
		Expect(writeSettingsByID(ctx, settings, edit)).To(MatchError("refused"))
		Expect(written).To(Equal([]string{"b=2", "b=2", "a=1"}))
	})

	It("compares the values the writes' bodies send, under their IDs", func() {
		body := func(value string) settingsTestBody { return settingsTestBody{data: `{"value":` + value + `}`} }
		got, err := settingsByIDJSON([]settingValue[string]{{id: "a", value: `"on"`}, {id: "o", value: `{"x":1}`}}, body)
		Expect(err).NotTo(HaveOccurred())
		Expect(got).To(MatchJSON(`{"a":"on","o":{"x":1}}`))

		_, err = settingsByIDJSON(settings[:1], func(string) settingsTestBody {
			return settingsTestBody{err: errors.New("no JSON")}
		})
		Expect(err).To(MatchError("no JSON"))
		_, err = settingsByIDJSON(settings[:1], func(string) settingsTestBody { return settingsTestBody{data: `[`} })
		Expect(err).To(HaveOccurred())
		_, err = settingsByIDJSON(settings[:1], func(string) settingsTestBody { return settingsTestBody{data: `{}`} })
		Expect(err).To(MatchError("the body of the setting b holds no value"))
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
