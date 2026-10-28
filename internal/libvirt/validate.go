package libvirt

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The libvirt/qemu tooling is executed directly (no intermediate shell), so the
// realistic risk is argument injection: a config-derived value that starts with
// "-" becomes an option of the invoked tool, and a value containing a comma
// corrupts the joined --disk/--network specs of virt-install. Every value that
// reaches an argv element is therefore checked against an allowlist before the
// command runs, so a broken config fails fast with a clear message instead of
// reaching the hypervisor tooling.

var (
	// argRe covers libvirt domain/network names, osinfo variants, volume IDs
	// and the non-flag argv elements of the virsh wrapper. It must not start
	// with "-" so a name can never be parsed as an option.
	argRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+@:/-]{0,253}$`)

	// pathRe covers disk, ISO and source paths. Spaces and shell metacharacters
	// are excluded on purpose: the lab layouts never use them and the stricter
	// set keeps the tools' option parsers unambiguous.
	pathRe = regexp.MustCompile(`^/?[A-Za-z0-9_][A-Za-z0-9._+/-]*$`)

	// macRe covers unicast/multicast MAC addresses as handed to virt-install.
	macRe = regexp.MustCompile(`^[A-Fa-f0-9]{2}(:[A-Fa-f0-9]{2}){5}$`)

	// uriRe covers the URI families the tool supports: the local system and
	// session sockets, the libvirt test driver used by the unit tests, and the
	// remote qemu+ssh endpoint. A config value outside these shapes would be
	// handed verbatim to the virsh probe and every later command, so it is
	// rejected up front.
	uriRe = regexp.MustCompile(`^qemu:///system$|^qemu:///session$|^test:///default$` +
		`|^qemu\+ssh://[A-Za-z0-9_][A-Za-z0-9._-]{0,62}@[A-Za-z0-9][A-Za-z0-9._-]{0,252}(/system|/session)?$` +
		`|^qemu\+ssh://[A-Za-z0-9][A-Za-z0-9._-]{0,252}(/system|/session)?$`)

	// extraArgsRe bounds the virt-install --extra-args payload, which
	// virt-install forwards to the installer kernel command line.
	extraArgsRe = regexp.MustCompile(`^[A-Za-z0-9 ._=/,:+-]*$`)

	// locationKeyRe/locationValueRe bound the key=value segments that may
	// follow an install location (e.g. "kernel=casper/vmlinuz" selects the
	// kernel virt-install boots from inside an ISO).
	locationKeyRe   = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]*$`)
	locationValueRe = regexp.MustCompile(`^[A-Za-z0-9._+/:@-]*$`)

	// diskFormats are the on-disk formats the lab may create.
	diskFormats = map[string]bool{"qcow2": true, "raw": true}
)

// ValidateURI reports whether uri is a libvirt URI this tool is allowed to run.
func ValidateURI(uri string) error {
	if uri == "" {
		return fmt.Errorf("libvirt URI is empty")
	}
	if !uriRe.MatchString(uri) {
		return fmt.Errorf("unsupported libvirt URI %q: use qemu:///system, qemu:///session or qemu+ssh://[user@]host[/system]", uri)
	}
	return nil
}

// validateArg reports whether value may be passed as a single non-flag argv
// element to the libvirt tooling.
func validateArg(field, value string) error {
	if !argRe.MatchString(value) {
		return fmt.Errorf("%s %q contains characters outside the allowed set (letters, digits and . _ + @ : / -)", field, value)
	}
	return nil
}

// validatePath reports whether value may be used as a file path argument.
func validatePath(field, value string) error {
	if len(value) > 4096 {
		return fmt.Errorf("%s exceeds the 4096 character path limit", field)
	}
	if !pathRe.MatchString(value) {
		return fmt.Errorf("%s %q contains characters outside the allowed set (letters, digits and . _ + / -)", field, value)
	}
	return nil
}

// validateDiskFormat restricts the format handed to qemu-img and virt-install.
func validateDiskFormat(field, value string) error {
	if !diskFormats[value] {
		return fmt.Errorf("%s %q is not one of the supported formats (qcow2, raw)", field, value)
	}
	return nil
}

// validateVirtInstallOpts checks every config-derived value that VirtInstall
// places into an argv element.
func validateVirtInstallOpts(opts VirtInstallOpts) error {
	if err := validateArg("guest name", opts.Name); err != nil {
		return err
	}
	if opts.OSVariant != "" && !argRe.MatchString(opts.OSVariant) {
		return fmt.Errorf("os-variant %q contains characters outside the allowed set", opts.OSVariant)
	}
	if opts.Location != "" {
		if err := validateInstallLocation(opts.Location); err != nil {
			return err
		}
	}
	if opts.ExtraArgs != "" {
		if len(opts.ExtraArgs) > 2048 {
			return fmt.Errorf("extra-args exceed the 2048 character limit")
		}
		if !extraArgsRe.MatchString(opts.ExtraArgs) {
			return fmt.Errorf("extra-args %q contains characters outside the allowed set", opts.ExtraArgs)
		}
	}
	for i, b := range opts.BootOrder {
		if !argRe.MatchString(b) {
			return fmt.Errorf("boot order entry %d (%q) contains characters outside the allowed set", i+1, b)
		}
	}
	for i, d := range opts.Disks {
		if err := validatePath(fmt.Sprintf("disk %d path", i+1), d.Path); err != nil {
			return err
		}
		if d.Format != "" {
			if err := validateDiskFormat(fmt.Sprintf("disk %d format", i+1), d.Format); err != nil {
				return err
			}
		}
		if d.Bus != "" && !argRe.MatchString(d.Bus) {
			return fmt.Errorf("disk %d bus %q contains characters outside the allowed set", i+1, d.Bus)
		}
		if d.Device != "" && !argRe.MatchString(d.Device) {
			return fmt.Errorf("disk %d device %q contains characters outside the allowed set", i+1, d.Device)
		}
	}
	for i, n := range opts.Networks {
		if !argRe.MatchString(n.Network) {
			return fmt.Errorf("network %d (%q) contains characters outside the allowed set", i+1, n.Network)
		}
		if n.Mac != "" && !macRe.MatchString(n.Mac) {
			return fmt.Errorf("network %d MAC %q is not a valid MAC address", i+1, n.Mac)
		}
	}
	return nil
}

// validateInstallLocation accepts an http(s) URL, an on-disk tree, or the
// virt-install combined form "<url-or-path>,kernel=...,initrd=..." used to
// boot directly from an ISO's kernel and initrd (the ISO jumpstart path).
func validateInstallLocation(location string) error {
	segments := strings.Split(location, ",")
	if err := validateLocationSource(segments[0]); err != nil {
		return err
	}
	for _, segment := range segments[1:] {
		key, value, ok := strings.Cut(segment, "=")
		if !ok || !locationKeyRe.MatchString(key) || !locationValueRe.MatchString(value) {
			return fmt.Errorf("install location option %q is not a supported key=value pair (allowed keys are option names with paths or flags)", segment)
		}
	}
	return nil
}

// validateLocationSource validates the URL or path part of an install location.
func validateLocationSource(source string) error {
	if strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://") {
		u, err := url.Parse(source)
		if err != nil || u.Host == "" {
			return fmt.Errorf("install location %q is not a valid URL", source)
		}
		return nil
	}
	return validatePath("install location", source)
}
