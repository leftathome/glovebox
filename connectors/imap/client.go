package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// realClient wraps go-imap/v2 to satisfy the IMAPClient interface.
type realClient struct {
	client *imapclient.Client
}

func newRealClient() IMAPClient {
	return &realClient{}
}

func (r *realClient) Connect(ctx context.Context) error {
	host := os.Getenv("IMAP_HOST")
	port := os.Getenv("IMAP_PORT")
	user := os.Getenv("IMAP_USERNAME")
	pass := os.Getenv("IMAP_PASSWORD")
	useTLS := os.Getenv("IMAP_TLS")

	if host == "" || user == "" || pass == "" {
		return fmt.Errorf("IMAP_HOST, IMAP_USERNAME, and IMAP_PASSWORD are required")
	}
	if port == "" {
		if strings.EqualFold(useTLS, "false") {
			port = "143"
		} else {
			port = "993"
		}
	}

	addr := net.JoinHostPort(host, port)

	var opts imapclient.Options
	if strings.EqualFold(useTLS, "false") {
		c, err := imapclient.DialInsecure(addr, &opts)
		if err != nil {
			return fmt.Errorf("dial insecure %s: %w", addr, err)
		}
		r.client = c
	} else {
		portNum, _ := strconv.Atoi(port)
		opts.TLSConfig = &tls.Config{
			ServerName: host,
			MinVersion: tls.VersionTLS12,
		}
		// Port 993 uses implicit TLS; other ports use STARTTLS.
		if portNum == 993 {
			c, err := imapclient.DialTLS(addr, &opts)
			if err != nil {
				return fmt.Errorf("dial TLS %s: %w", addr, err)
			}
			r.client = c
		} else {
			c, err := imapclient.DialStartTLS(addr, &opts)
			if err != nil {
				return fmt.Errorf("dial STARTTLS %s: %w", addr, err)
			}
			r.client = c
		}
	}

	if err := r.client.Login(user, pass).Wait(); err != nil {
		r.client.Close()
		return fmt.Errorf("login: %w", err)
	}

	return nil
}

func (r *realClient) SelectFolder(ctx context.Context, folder string) error {
	if _, err := r.client.Select(folder, nil).Wait(); err != nil {
		return fmt.Errorf("select %q: %w", folder, err)
	}
	return nil
}

func (r *realClient) SearchSinceUID(ctx context.Context, uid uint32) ([]uint32, error) {
	// Search for UIDs greater than the given UID.
	criteria := &imap.SearchCriteria{
		UID: []imap.UIDSet{
			{imap.UIDRange{Start: imap.UID(uid + 1), Stop: 0}},
		},
	}

	data, err := r.client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, fmt.Errorf("uid search: %w", err)
	}

	var uids []uint32
	for _, uidSet := range data.AllUIDs() {
		uids = append(uids, uint32(uidSet))
	}

	return uids, nil
}

func (r *realClient) FetchMessage(ctx context.Context, uid uint32) ([]byte, string, string, time.Time, error) {
	bodySection := &imap.FetchItemBodySection{
		Specifier: imap.PartSpecifierNone,
		Peek:      true,
	}

	fetchOpts := &imap.FetchOptions{
		Envelope:    true,
		BodySection: []*imap.FetchItemBodySection{bodySection},
	}

	uidSet := imap.UIDSet{imap.UIDRange{Start: imap.UID(uid), Stop: imap.UID(uid)}}
	msgs, err := r.client.Fetch(uidSet, fetchOpts).Collect()
	if err != nil {
		return nil, "", "", time.Time{}, fmt.Errorf("fetch uid %d: %w", uid, err)
	}
	if len(msgs) == 0 {
		return nil, "", "", time.Time{}, fmt.Errorf("no message returned for uid %d", uid)
	}

	msg := msgs[0]

	var sender string
	var subject string
	var date time.Time
	if msg.Envelope != nil {
		subject = msg.Envelope.Subject
		date = msg.Envelope.Date
		if len(msg.Envelope.From) > 0 {
			from := msg.Envelope.From[0]
			if from.Name != "" {
				sender = from.Name
			} else {
				sender = from.Mailbox + "@" + from.Host
			}
		}
	}

	raw := msg.FindBodySection(bodySection)
	if raw == nil {
		// If BODY[] was not returned, reconstruct minimal raw from envelope.
		var buf bytes.Buffer
		fmt.Fprintf(&buf, "From: %s\r\nSubject: %s\r\nDate: %s\r\n\r\n",
			sender, subject, date.Format(time.RFC1123Z))
		raw = buf.Bytes()
	}

	return raw, sender, subject, date, nil
}

func (r *realClient) Idle(ctx context.Context) error {
	idleCmd, err := r.client.Idle()
	if err != nil {
		return fmt.Errorf("start idle: %w", err)
	}

	// Wait until context is cancelled or the server sends an update.
	select {
	case <-ctx.Done():
	}

	if err := idleCmd.Close(); err != nil {
		return fmt.Errorf("close idle: %w", err)
	}
	return ctx.Err()
}

func (r *realClient) Close() error {
	if r.client != nil {
		return r.client.Close()
	}
	return nil
}
