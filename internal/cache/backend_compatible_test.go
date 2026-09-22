//go:build compatibletests

package cache

import "aos-cx-docs-dldr/internal/fetch"

var newContractClient = fetch.NewCompatible
