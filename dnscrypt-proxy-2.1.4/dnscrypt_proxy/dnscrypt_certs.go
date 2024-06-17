package dnscrypt_proxy

import (
	"bytes"
	"encoding/binary"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/miekg/dns"
	"golang.org/x/crypto/ed25519"
)

type CertInfo struct {
	ServerPk           [32]byte
	SharedKey          [32]byte
	MagicQuery         [ClientMagicLen]byte
	CryptoConstruction CryptoConstruction
	ForwardSecurity    bool
}

func FetchCurrentDNSCryptCert(
	proxy *Proxy,
	serverName *string,
	proto string,
	pk ed25519.PublicKey,
	serverAddress string,
	providerName string,
	isNew bool,
	relay *DNSCryptRelay,
	knownBugs ServerBugs,
) (CertInfo, int, bool, error) {
	if len(pk) != ed25519.PublicKeySize {
		return CertInfo{}, 0, false, errors.New("Invalid public key length")
	}
	if !strings.HasSuffix(providerName, ".") {
		providerName = providerName + "."
	}
	if serverName == nil {
		serverName = &providerName
	}
	query := dns.Msg{}
	query.SetQuestion(providerName, dns.TypeTXT)
	if !strings.HasPrefix(providerName, "2.dnscrypt-cert.") {
		if relay != nil && !proxy.anonDirectCertFallback {
			log.Printf(
				"[%v] uses a non-standard provider name, enable direct cert fallback to use with a relay ('%v' doesn't start with '2.dnscrypt-cert.')",
				*serverName,
				providerName,
			)
		} else {
			log.Printf("[%v] uses a non-standard provider name ('%v' doesn't start with '2.dnscrypt-cert.')", *serverName, providerName)
			relay = nil
		}
	}
	tryFragmentsSupport := true
	if knownBugs.fragmentsBlocked {
		tryFragmentsSupport = false
	}
	in, rtt, fragmentsBlocked, err := DNSExchange(
		proxy,
		proto,
		&query,
		serverAddress,
		relay,
		serverName,
		tryFragmentsSupport,
	)
	if err != nil {
		log.Printf("[%s] TIMEOUT", *serverName)
		return CertInfo{}, 0, fragmentsBlocked, err
	}
	now := uint32(time.Now().Unix())
	certInfo := CertInfo{CryptoConstruction: UndefinedConstruction}
	highestSerial := uint32(0)
	var certCountStr string
	for _, answerRr := range in.Answer {
		var txt string
		if t, ok := answerRr.(*dns.TXT); !ok {
			log.Printf("[%v] Extra record of type [%v] found in certificate", *serverName, answerRr.Header().Rrtype)
			continue
		} else {
			txt = strings.Join(t.Txt, "")
		}
		binCert := PackTXTRR(txt)
		if len(binCert) < 124 {
			log.Printf("[%v] Certificate too short", *serverName)
			continue
		}
		if !bytes.Equal(binCert[:4], CertMagic[:4]) {
			log.Printf("[%v] Invalid cert magic", *serverName)
			continue
		}
		cryptoConstruction := CryptoConstruction(0)
		switch esVersion := binary.BigEndian.Uint16(binCert[4:6]); esVersion {
		case 0x0001:
			cryptoConstruction = XSalsa20Poly1305
		case 0x0002:
			cryptoConstruction = XChacha20Poly1305
		default:
			log.Printf("[%v] Unsupported crypto construction", *serverName)
			continue
		}
		signature := binCert[8:72]
		signed := binCert[72:]
		if !ed25519.Verify(pk, signed, signature) {
			log.Printf("[%v] Incorrect signature for provider name: [%v]", *serverName, providerName)
			continue
		}
		serial := binary.BigEndian.Uint32(binCert[112:116])
		tsBegin := binary.BigEndian.Uint32(binCert[116:120])
		tsEnd := binary.BigEndian.Uint32(binCert[120:124])
		if tsBegin >= tsEnd {
			log.Printf("[%v] certificate ends before it starts (%v >= %v)", *serverName, tsBegin, tsEnd)
			continue
		}
		ttl := tsEnd - tsBegin
		if ttl > 86400*7 {
			log.Printf(
				"[%v] the key validity period for this server is excessively long (%d days), significantly reducing reliability and forward security.",
				*serverName,
				ttl/86400,
			)
			daysLeft := (tsEnd - now) / 86400
			if daysLeft < 1 {
				log.Printf(
					"[%v] certificate will expire today -- Switch to a different resolver as soon as possible",
					*serverName,
				)
			} else if daysLeft <= 7 {
				log.Printf("[%v] certificate is about to expire -- if you don't manage this server, tell the server operator about it", *serverName)
			} else if daysLeft <= 30 {
				log.Printf("[%v] certificate will expire in %d days", *serverName, daysLeft)
			} else {
				log.Printf("[%v] certificate still valid for %d days", *serverName, daysLeft)
			}
			certInfo.ForwardSecurity = false
		} else {
			certInfo.ForwardSecurity = true
		}
		if !proxy.certIgnoreTimestamp {
			if now > tsEnd || now < tsBegin {
				log.Printf(
					"[%v] Certificate not valid at the current date (now: %v is not in [%v..%v])",
					*serverName,
					now,
					tsBegin,
					tsEnd,
				)
				continue
			}
		}
		if serial < highestSerial {
			log.Printf("[%v] Superseded by a previous certificate", *serverName)
			continue
		}
		if serial == highestSerial {
			if cryptoConstruction < certInfo.CryptoConstruction {
				log.Printf("[%v] Keeping the previous, preferred crypto construction", *serverName)
				continue
			} else {
				log.Printf("[%v] Upgrading the construction from %v to %v", *serverName, certInfo.CryptoConstruction, cryptoConstruction)
			}
		}
		if cryptoConstruction != XChacha20Poly1305 && cryptoConstruction != XSalsa20Poly1305 {
			log.Printf("[%v] Cryptographic construction %v not supported", *serverName, cryptoConstruction)
			continue
		}
		var serverPk [32]byte
		copy(serverPk[:], binCert[72:104])
		sharedKey := ComputeSharedKey(cryptoConstruction, &proxy.proxySecretKey, &serverPk, &providerName)
		certInfo.SharedKey = sharedKey
		highestSerial = serial
		certInfo.CryptoConstruction = cryptoConstruction
		copy(certInfo.ServerPk[:], serverPk[:])
		copy(certInfo.MagicQuery[:], binCert[104:112])
		if isNew {
			log.Printf("[%s] OK (DNSCrypt) - rtt: %dms%s", *serverName, rtt.Nanoseconds()/1000000, certCountStr)
		} else {
			log.Printf("[%s] OK (DNSCrypt) - rtt: %dms%s", *serverName, rtt.Nanoseconds()/1000000, certCountStr)
		}
		certCountStr = " - additional certificate"
	}
	if certInfo.CryptoConstruction == UndefinedConstruction {
		return certInfo, 0, fragmentsBlocked, errors.New("No useable certificate found")
	}
	return certInfo, int(rtt.Nanoseconds() / 1000000), fragmentsBlocked, nil
}
