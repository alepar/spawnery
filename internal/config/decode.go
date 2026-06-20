package config

import (
	"github.com/go-viper/mapstructure/v2"
	"github.com/knadh/koanf/v2"
)

// decodeInto unmarshals the merged koanf map into dst using a single decode pass with
// WeaklyTypedInput (so the string values produced by the env/--set layers coerce to the schema's
// int/bool types) plus a DecodeHook chain. WeaklyTypedInput does NOT cover string→time.Duration
// on its own (per spike S1), so StringToTimeDurationHookFunc is wired explicitly; TextUnmarshaller
// lets custom types (e.g. net/netip) parse from strings. An uncoercible value surfaces as an
// error naming the offending field.
func decodeInto(k *koanf.Koanf, dst any) error {
	return k.UnmarshalWithConf("", dst, koanf.UnmarshalConf{
		Tag: "koanf",
		DecoderConfig: &mapstructure.DecoderConfig{
			Result:           dst,
			TagName:          "koanf",
			WeaklyTypedInput: true,
			DecodeHook: mapstructure.ComposeDecodeHookFunc(
				mapstructure.StringToTimeDurationHookFunc(),
				mapstructure.StringToSliceHookFunc(","),
				mapstructure.TextUnmarshallerHookFunc(),
			),
		},
	})
}
