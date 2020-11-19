// Copyright (C) 2020 Storj Labs, Inc.
// See LICENSE for copying information.

package sharing

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestCompareHosts(t *testing.T) {
	same := [][2]string{
		{"website.test", "website.test"},
		{"website.test:443", "website.test"},
		{"website.test:443", "website.test:443"},
		{"website.test:443", "website.test:880"},
		{"192.168.0.1:443", "192.168.0.1:880"},
		{"[::1]:443", "[::1]:880"},
	}
	for _, test := range same {
		result, err := compareHosts(test[0], test[1])
		assert.NoError(t, err)
		assert.True(t, result)
	}

	notsame := [][2]string{
		{"website.test:443", "site.test:443"},
		{"website.test", "site.test"},
		{"[::1]:443", "[::2]:880"},
	}
	for _, test := range notsame {
		result, err := compareHosts(test[0], test[1])
		assert.NoError(t, err)
		assert.False(t, result)
	}
}

func TestDetermineBucketAndObjectKey(t *testing.T) {
	for idx, test := range []struct {
		name          string
		root, urlPath string
		bucket, key   string
	}{
		{
			name:    "simple",
			root:    "bucket/prefix/",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "prefix/images/pic.jpg",
		},
		{
			name:    "standalone bucket",
			root:    "bucket",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "images/pic.jpg",
		},
		{
			name:    "bucket with slash",
			root:    "bucket/",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "images/pic.jpg",
		},
		{
			name:    "bucket with slash as prefix",
			root:    "bucket//",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "/images/pic.jpg",
		},
		{
			name:    "bucket with two slashes as prefix but no trailing slash",
			root:    "bucket//prefix",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "/prefix/images/pic.jpg",
		},
		{
			name:    "bucket with two slashes after prefix",
			root:    "bucket/prefix//",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "prefix//images/pic.jpg",
		},
		{
			name:    "prefix with no slash",
			root:    "bucket/prefix",
			urlPath: "/images/pic.jpg",
			bucket:  "bucket",
			key:     "prefix/images/pic.jpg",
		},
		{
			name:    "url with two slashes",
			root:    "bucket/prefix/",
			urlPath: "//images/pic.jpg",
			bucket:  "bucket",
			key:     "prefix//images/pic.jpg",
		},
	} {
		actualBucket, actualKey := determineBucketAndObjectKey(test.root, test.urlPath)
		assert.Equal(t, actualBucket, test.bucket, fmt.Sprintf("%d: %s", idx, test.name))
		assert.Equal(t, actualKey, test.key, fmt.Sprintf("%d: %s", idx, test.name))
	}
}
