package json5

import "fmt"

// Options are local safety controls used by the documentation inventory reader.
type Options struct {
	UseNumber             bool
	DisallowDuplicateKeys bool
	MaxDepth              int
	MaxValues             int
	MaxBytes              int
}

func UnmarshalWithOptions(data []byte, v interface{}, options Options) error {
	if options.MaxDepth < 0 || options.MaxValues < 0 || options.MaxBytes < 0 {
		return fmt.Errorf("negative JSON5 limit")
	}
	if options.MaxBytes > 0 && len(data) > options.MaxBytes {
		return fmt.Errorf("JSON5 byte limit exceeded")
	}
	normalized, err := normalizeInput(data)
	if err != nil {
		return err
	}
	d := decodeState{useNumber: options.UseNumber, disallowDuplicateKeys: options.DisallowDuplicateKeys}
	d.scan.maxDepth, d.scan.maxValues = options.MaxDepth, options.MaxValues
	d.nextscan.maxDepth, d.nextscan.maxValues = options.MaxDepth, options.MaxValues
	if err := checkValid(normalized, &d.scan); err != nil {
		return err
	}
	d.init(normalized)
	return d.unmarshal(v)
}
