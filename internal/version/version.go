// Package version holds values injected at build time with -ldflags -X.
package version

var (
	// Version is this build's release version, MAJOR.MINOR.PATCH.
	Version = "0.0.0"

	// PublicKeys is the trusted release-signing key ring, formatted as
	// "keyid:base64pubkey[,keyid:base64pubkey...]". A build without keys
	// refuses to self-update.
	PublicKeys = ""
)
