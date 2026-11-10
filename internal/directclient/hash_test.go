package directclient

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"testing"
)

func TestMagnetHashHexAndBase32(t *testing.T) {
	const hash = "0123456789abcdef0123456789abcdef01234567"
	decoded, err := hex.DecodeString(hash)
	if err != nil {
		t.Fatal(err)
	}
	for _, magnet := range []string{
		"magnet:?xt=urn:btih:" + hash + "&dn=Episode",
		"magnet:?xt=urn:btih:" + base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(decoded),
		// URI schemes are case-insensitive (RFC 3986); these spellings must
		// behave identically to lowercase magnet:.
		"MAGNET:?xt=urn:btih:" + hash + "&dn=Episode",
		"Magnet:?xt=urn:btih:" + hash,
	} {
		got, err := magnetHash(magnet)
		if err != nil || got != hash {
			t.Fatalf("magnetHash(%q) = %q, %v; want %q", magnet, got, err, hash)
		}
	}
	if _, err := magnetHash("magnet:?xt=urn:btih:invalid"); err == nil {
		t.Fatal("accepted invalid BTIH hash")
	}
}

func TestTorrentHashUsesOriginalInfoBytes(t *testing.T) {
	// Hash the exact bencoded info dictionary; neither the surrounding
	// announce URL nor a decoded/re-encoded dictionary belongs in the hash.
	info := []byte("d4:name7:episode6:lengthi12ee")
	torrent := append([]byte("d8:announce7:tracker4:info"), info...)
	torrent = append(torrent, 'e')
	want := sha1.Sum(info)
	got, err := torrentHash(torrent)
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatalf("torrentHash = %q, %v; want %x", got, err, want)
	}
	if _, err := torrentHash([]byte("d4:infod4:name10:truncated")); err == nil {
		t.Fatal("accepted truncated torrent")
	}
}

// TestTorrentHashRejectsMalformedTopLevel verifies that the top-level
// dictionary must be exactly one closed bencoded dictionary: a truncated
// file (missing final 'e') and a file with appended garbage are rejected
// instead of queued.
func TestTorrentHashRejectsMalformedTopLevel(t *testing.T) {
	info := "d4:name7:episode6:lengthi12ee"
	valid := []byte("d8:announce7:tracker4:info" + info + "e")
	if _, err := torrentHash(valid); err != nil {
		t.Fatalf("valid torrent rejected: %v", err)
	}
	// Truncated: the info dictionary parsed fine but the top-level
	// dictionary never closes.
	truncated := []byte("d8:announce7:tracker4:info" + info)
	if _, err := torrentHash(truncated); err == nil {
		t.Fatal("accepted torrent whose top-level dictionary is not closed")
	}
	// Garbage appended after the closing 'e'.
	appended := append(append([]byte{}, valid...), 'G')
	if _, err := torrentHash(appended); err == nil {
		t.Fatal("accepted torrent with data after the top-level dictionary")
	}
}
