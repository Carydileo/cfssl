// Package local implements certificate signature functionality for CF-SSL.
package local

import (
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/ioutil"
	"net"

	"github.com/cloudflare/cfssl/config"
	cferr "github.com/cloudflare/cfssl/errors"
	"github.com/cloudflare/cfssl/helpers"
	"github.com/cloudflare/cfssl/log"
	"github.com/cloudflare/cfssl/signer"
)

// Signer contains a signer that uses the standard library to
// support both ECDSA and RSA CA keys.
type Signer struct {
	ca      *x509.Certificate
	priv    crypto.Signer
	policy  *config.Signing
	sigAlgo x509.SignatureAlgorithm
}

// NewSigner creates a new Signer directly from a
// private key and certificate, with optional policy.
func NewSigner(priv crypto.Signer, cert *x509.Certificate, sigAlgo x509.SignatureAlgorithm, policy *config.Signing) (*Signer, error) {
	if policy == nil {
		policy = &config.Signing{
			Profiles: map[string]*config.SigningProfile{},
			Default:  config.DefaultConfig()}
	}

	if !policy.Valid() {
		return nil, cferr.New(cferr.PolicyError, cferr.InvalidPolicy)
	}

	return &Signer{
		ca:      cert,
		priv:    priv,
		sigAlgo: sigAlgo,
		policy:  policy,
	}, nil
}

// NewSignerFromFile generates a new local signer from a caFile
// and a caKey file, both PEM encoded.
func NewSignerFromFile(caFile, caKeyFile string, policy *config.Signing) (*Signer, error) {
	log.Debug("Loading CA: ", caFile)
	ca, err := ioutil.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	log.Debug("Loading CA key: ", caKeyFile)
	cakey, err := ioutil.ReadFile(caKeyFile)
	if err != nil {
		return nil, cferr.Wrap(cferr.CertificateError, cferr.ReadFailed, err)
	}

	parsedCa, err := helpers.ParseCertificatePEM(ca)
	if err != nil {
		return nil, err
	}

	priv, err := helpers.ParsePrivateKeyPEM(cakey)
	if err != nil {
		log.Debug("Malformed private key %v", err)
		return nil, err
	}

	return NewSigner(priv, parsedCa, signer.DefaultSigAlgo(priv), policy)
}

func (s *Signer) sign(template *x509.Certificate, profile *config.SigningProfile, serialSeq string) (cert []byte, err error) {
	err = signer.FillTemplate(template, s.policy.Default, profile, serialSeq)
	if err != nil {
		return
	}

	serialNumber := template.SerialNumber
	var initRoot bool
	if s.ca == nil {
		if !template.IsCA {
			err = cferr.New(cferr.PolicyError, cferr.InvalidRequest)
			return
		}
		template.DNSNames = nil
		s.ca = template
		initRoot = true
		template.MaxPathLen = signer.MaxPathLen
	} else if template.IsCA {
		template.MaxPathLen = 1
		template.DNSNames = nil
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, template, s.ca, template.PublicKey, s.priv)
	if err != nil {
		return nil, cferr.Wrap(cferr.CertificateError, cferr.Unknown, err)
	}
	if initRoot {
		s.ca, err = x509.ParseCertificate(derBytes)
		if err != nil {
			return nil, cferr.Wrap(cferr.CertificateError, cferr.ParseFailed, err)
		}
	}

	cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	log.Infof("signed certificate with serial number %s", serialNumber)
	return
}

// replaceSliceIfEmpty replaces the contents of replaced with newContents if
// the slice referenced by replaced is empty
func replaceSliceIfEmpty(replaced, newContents *[]string) {
	if len(*replaced) == 0 {
		*replaced = *newContents
	}
}

func whitelistString(keep bool, field *string) {
	if !keep {
		*field = ""
	}
}

func whitelistStringSlice(keep bool, field *[]string) {
	if !keep {
		*field = []string{}
	}
}

// whitelistRequest checks the request for a whitelist. If one isn't
// present, the name is untouched. If it is present, only those fields
// which are explictly permitted are kept.
func whitelistRequest(s *signer.Subject, name pkix.Name) pkix.Name {
	if s == nil || s.Whitelist == nil {
		return name
	}

	whitelistString(s.Whitelist.CN, &name.CommonName)
	whitelistStringSlice(s.Whitelist.C, &name.Country)
	whitelistStringSlice(s.Whitelist.ST, &name.Province)
	whitelistStringSlice(s.Whitelist.L, &name.Locality)
	whitelistStringSlice(s.Whitelist.O, &name.Organization)
	whitelistStringSlice(s.Whitelist.OU, &name.OrganizationalUnit)

	return name
}

// PopulateSubjectFromCSR has functionality similar to Name, except
// it fills the fields of the resulting pkix.Name with req's if the
// subject's corresponding fields are empty
func PopulateSubjectFromCSR(s *signer.Subject, req pkix.Name) pkix.Name {
	// if no subject, use req
	if s == nil {
		return req
	}

	req = whitelistRequest(s, req)
	if len(s.Names) == 0 {
		return req
	}
	name := s.Name()

	if name.CommonName == "" {
		name.CommonName = req.CommonName
	}

	replaceSliceIfEmpty(&name.Country, &req.Country)
	replaceSliceIfEmpty(&name.Province, &req.Province)
	replaceSliceIfEmpty(&name.Locality, &req.Locality)
	replaceSliceIfEmpty(&name.Organization, &req.Organization)
	replaceSliceIfEmpty(&name.OrganizationalUnit, &req.OrganizationalUnit)

	return name
}

// OverrideHosts fills template's IPAddresses and DNSNames with the
// content of hosts, if it is not nil.
func OverrideHosts(template *x509.Certificate, hosts []string) {
	if hosts != nil {
		template.IPAddresses = []net.IP{}
		template.DNSNames = []string{}
	}

	for i := range hosts {
		if ip := net.ParseIP(hosts[i]); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, hosts[i])
		}
	}

}

// Sign signs a new certificate based on the PEM-encoded client
// certificate or certificate request with the signing profile,
// specified by profileName.
func (s *Signer) Sign(req signer.SignRequest) (cert []byte, err error) {
	profile := s.policy.Profiles[req.Profile]
	if profile == nil {
		profile = s.policy.Default
	}

	serialSeq := ""
	if profile.UseSerialSeq {
		serialSeq = req.SerialSeq
	}

	block, _ := pem.Decode([]byte(req.Request))
	if block == nil {
		return nil, cferr.New(cferr.CertificateError, cferr.DecodeFailed)
	}

	if block.Type != "CERTIFICATE REQUEST" {
		return nil, cferr.Wrap(cferr.CertificateError,
			cferr.BadRequest, errors.New("not a certificate or csr"))
	}

	template, err := signer.ParseCertificateRequest(s, block.Bytes)
	if err != nil {
		return nil, err
	}

	OverrideHosts(template, req.Hosts)
	template.Subject = PopulateSubjectFromCSR(req.Subject, template.Subject)

	return s.sign(template, profile, serialSeq)
}

// SigAlgo returns the RSA signer's signature algorithm.
func (s *Signer) SigAlgo() x509.SignatureAlgorithm {
	return s.sigAlgo
}

// Certificate returns the signer's certificate.
func (s *Signer) Certificate(label, profile string) (*x509.Certificate, error) {
	cert := *s.ca
	return &cert, nil
}

// SetPolicy sets the signer's signature policy.
func (s *Signer) SetPolicy(policy *config.Signing) {
	s.policy = policy
}

// Policy returns the signer's policy.
func (s *Signer) Policy() *config.Signing {
	return s.policy
}
