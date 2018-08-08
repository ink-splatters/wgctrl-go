//+build linux

package wgnl

import (
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/genetlink"
	"github.com/mdlayher/genetlink/genltest"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nlenc"
	"github.com/mdlayher/netlink/nltest"
	"github.com/mdlayher/wireguardctrl/internal/wgnl/internal/wgh"
	"github.com/mdlayher/wireguardctrl/wgtypes"
	"golang.org/x/sys/unix"
)

const (
	okIndex = 1
	okName  = "wg0"
)

func TestLinuxClientDevicesEmpty(t *testing.T) {
	tests := []struct {
		name string
		fn   func() ([]string, error)
	}{
		{
			name: "no interfaces",
			fn: func() ([]string, error) {
				return nil, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, func(_ genetlink.Message, _ netlink.Message) ([]genetlink.Message, error) {
				panic("no devices; shouldn't call genetlink")
			})
			defer c.Close()

			c.interfaces = tt.fn

			ds, err := c.Devices()
			if err != nil {
				t.Fatalf("failed to get devices: %v", err)
			}

			if diff := cmp.Diff(0, len(ds)); diff != "" {
				t.Fatalf("unexpected number of devices (-want +got):\n%s", diff)
			}
		})
	}
}

func TestLinuxClientIsNotExist(t *testing.T) {
	byIndex := func(c *client) error {
		_, err := c.DeviceByIndex(1)
		return err
	}

	byName := func(c *client) error {
		_, err := c.DeviceByName("wg0")
		return err
	}

	configure := func(c *client) error {
		return c.ConfigureDevice("wg0", wgtypes.Config{})
	}

	tests := []struct {
		name string
		fn   func(c *client) error
		msgs []genetlink.Message
		err  error
	}{
		{
			name: "index: 0",
			fn: func(c *client) error {
				_, err := c.DeviceByIndex(0)
				return err
			},
		},
		{
			name: "name: empty",
			fn: func(c *client) error {
				_, err := c.DeviceByName("")
				return err
			},
		},
		{
			name: "index: ENODEV",
			fn:   byIndex,
			err:  unix.ENODEV,
		},
		{
			name: "index: ENOTSUP",
			fn:   byIndex,
			err:  unix.ENOTSUP,
		},
		{
			name: "name: ENODEV",
			fn:   byName,
			err:  unix.ENODEV,
		},
		{
			name: "name: ENOTSUP",
			fn:   byName,
			err:  unix.ENOTSUP,
		},
		{
			name: "configure: ENODEV",
			fn:   configure,
			err:  unix.ENODEV,
		},
		{
			name: "configure: ENOTSUP",
			fn:   configure,
			err:  unix.ENOTSUP,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := testClient(t, func(_ genetlink.Message, _ netlink.Message) ([]genetlink.Message, error) {
				return tt.msgs, tt.err
			})
			defer c.Close()

			if err := tt.fn(c); !os.IsNotExist(err) {
				t.Fatalf("expected is not exist, but got: %v", err)
			}
		})
	}
}

func Test_parseRTNLInterfaces(t *testing.T) {
	// marshalAttrs creates packed netlink attributes with a prepended ifinfomsg
	// structure, as returned by rtnetlink.
	marshalAttrs := func(attrs []netlink.Attribute) []byte {
		ifinfomsg := make([]byte, syscall.SizeofIfInfomsg)

		return append(ifinfomsg, nltest.MustMarshalAttributes(attrs)...)
	}

	tests := []struct {
		name string
		msgs []syscall.NetlinkMessage
		ifis []string
		ok   bool
	}{
		{
			name: "short ifinfomsg",
			msgs: []syscall.NetlinkMessage{{
				Header: syscall.NlMsghdr{
					Type: unix.RTM_NEWLINK,
				},
				Data: []byte{0xff},
			}},
		},
		{
			name: "empty",
			ok:   true,
		},
		{
			name: "immediate done",
			msgs: []syscall.NetlinkMessage{{
				Header: syscall.NlMsghdr{
					Type: unix.NLMSG_DONE,
				},
			}},
			ok: true,
		},
		{
			name: "ok",
			msgs: []syscall.NetlinkMessage{
				// Bridge device.
				{
					Header: syscall.NlMsghdr{
						Type: unix.RTM_NEWLINK,
					},
					Data: marshalAttrs([]netlink.Attribute{
						{
							Type: unix.IFLA_IFNAME,
							Data: nlenc.Bytes("br0"),
						},
						{
							Type: unix.IFLA_LINKINFO,
							Data: nltest.MustMarshalAttributes([]netlink.Attribute{{
								Type: unix.IFLA_INFO_KIND,
								Data: nlenc.Bytes("bridge"),
							}}),
						},
					}),
				},
				// WireGuard device.
				{
					Header: syscall.NlMsghdr{
						Type: unix.RTM_NEWLINK,
					},
					Data: marshalAttrs([]netlink.Attribute{
						{
							Type: unix.IFLA_IFNAME,
							Data: nlenc.Bytes(okName),
						},
						{
							Type: unix.IFLA_LINKINFO,
							Data: nltest.MustMarshalAttributes([]netlink.Attribute{
								// Random junk to skip.
								{
									Type: 255,
									Data: nlenc.Uint16Bytes(0xff),
								},
								{
									Type: unix.IFLA_INFO_KIND,
									Data: nlenc.Bytes(wgKind),
								},
							}),
						},
					}),
				},
			},
			ifis: []string{okName},
			ok:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ifis, err := parseRTNLInterfaces(tt.msgs)

			if tt.ok && err != nil {
				t.Fatalf("failed to parse interfaces: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatal("expected an error, but none occurred")
			}
			if err != nil {
				return
			}

			if diff := cmp.Diff(tt.ifis, ifis); diff != "" {
				t.Fatalf("unexpected interfaces (-want +got):\n%s", diff)
			}
		})
	}
}

const familyID = 20

func testClient(t *testing.T, fn genltest.Func) *client {
	family := genetlink.Family{
		ID:      familyID,
		Version: wgh.GenlVersion,
		Name:    wgh.GenlName,
	}

	conn := genltest.Dial(genltest.ServeFamily(family, fn))

	c, err := initClient(conn)
	if err != nil {
		t.Fatalf("failed to open client: %v", err)
	}

	c.interfaces = func() ([]string, error) {
		return []string{okName}, nil
	}

	return c
}

func diffAttrs(x, y []netlink.Attribute) string {
	// Make copies to avoid a race and then zero out length values
	// for comparison.
	xPrime := make([]netlink.Attribute, len(x))
	copy(xPrime, x)

	for i := 0; i < len(xPrime); i++ {
		xPrime[i].Length = 0
	}

	yPrime := make([]netlink.Attribute, len(y))
	copy(yPrime, y)

	for i := 0; i < len(yPrime); i++ {
		yPrime[i].Length = 0
	}

	return cmp.Diff(xPrime, yPrime)
}

func mustCIDR(s string) net.IPNet {
	_, cidr, err := net.ParseCIDR(s)
	if err != nil {
		panicf("failed to parse CIDR: %v", err)
	}

	return *cidr
}

func mustAllowedIPs(ipns []net.IPNet) []byte {
	var attrs []netlink.Attribute
	for i, ipn := range ipns {
		var (
			ip     = ipn.IP
			family = uint16(unix.AF_INET6)
		)

		if ip4 := ip.To4(); ip4 != nil {
			ip = ip4
			family = unix.AF_INET
		}

		ones, _ := ipn.Mask.Size()

		data := nltest.MustMarshalAttributes([]netlink.Attribute{
			{
				Type: wgh.AllowedipAFamily,
				Data: nlenc.Uint16Bytes(family),
			},
			{
				Type: wgh.AllowedipAIpaddr,
				Data: ip,
			},
			{
				Type: wgh.AllowedipACidrMask,
				Data: nlenc.Uint8Bytes(uint8(ones)),
			},
		})

		attrs = append(attrs, netlink.Attribute{
			Type: uint16(i),
			Data: data,
		})
	}

	return nltest.MustMarshalAttributes(attrs)
}

func mustPrivateKey() wgtypes.Key {
	k, err := wgtypes.GeneratePrivateKey()
	if err != nil {
		panicf("failed to generate private key: %v", err)
	}

	return k
}

func mustPublicKey() wgtypes.Key {
	return mustPrivateKey().PublicKey()
}

func intPtr(v int) *int {
	return &v
}

func panicf(format string, a ...interface{}) {
	panic(fmt.Sprintf(format, a...))
}

func durPtr(d time.Duration) *time.Duration {
	return &d
}

func keyPtr(k wgtypes.Key) *wgtypes.Key {
	return &k
}

func keyBytes(k wgtypes.Key) []byte {
	return k[:]
}

func mustHexKey(s string) wgtypes.Key {
	b, err := hex.DecodeString(s)
	if err != nil {
		panicf("failed to decode hex key: %v", err)
	}

	k, err := wgtypes.NewKey(b)
	if err != nil {
		panicf("failed to create key: %v", err)
	}

	return k
}

func mustUDPAddr(s string) *net.UDPAddr {
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		panicf("failed to resolve UDP address: %v", err)
	}

	return a
}
