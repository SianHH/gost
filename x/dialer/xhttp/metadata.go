package xhttp

import (
	mdata "github.com/go-gost/core/metadata"
	xhttp_util "github.com/go-gost/x/internal/util/xhttp"
)

type metadata struct {
	config xhttp_util.Config
}

func (d *xhttpDialer) parseMetadata(md mdata.Metadata) (err error) {
	d.md.config = xhttp_util.ConfigFromMetadata(md)
	return nil
}
