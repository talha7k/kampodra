// Package cloud is the provider-neutral adapter for cloud CLIs (oci, gcp):
// DNS records, object-storage backups, blue/green, image import.
// Per-provider legs live in *_oci.go / *_gcp.go / dnsops_*.go behind
// dnsops.go + provider.go.
package cloud
