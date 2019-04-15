package kubernetesidp

import (
	"crypto/ecdsa"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"

	"github.com/briankassouf/jose/crypto"
	"github.com/briankassouf/jose/jws"
	"github.com/briankassouf/jose/jwt"
	"github.com/hashicorp/consul/agent/structs"
	"github.com/hashicorp/errwrap"
	cleanhttp "github.com/hashicorp/go-cleanhttp"
	multierror "github.com/hashicorp/go-multierror"
	"github.com/mitchellh/mapstructure"
	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	client_metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8s "k8s.io/client-go/kubernetes"
	client_authv1 "k8s.io/client-go/kubernetes/typed/authentication/v1"
	client_corev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	client_rest "k8s.io/client-go/rest"
)

const (
	// expectedJWTIssuer is used to verify the iss header on the JWT.
	expectedJWTIssuer = "kubernetes/serviceaccount"

	uidJWTClaimKey = "kubernetes.io/serviceaccount/service-account.uid"

	serviceAccountServiceNameAnnotation = "consul.hashicorp.com/service-name"
)

// errMismatchedSigningMethod is used if the certificate doesn't match the
// JWT's expected signing method.
var errMismatchedSigningMethod = errors.New("invalid signing method")

func ValidateJWT(jwtStr string) error {
	_, err := jws.ParseJWT([]byte(jwtStr))
	return err
}

// ParsePublicKeyPEM is used to parse RSA and ECDSA public keys from PEMs
func ParsePublicKeyPEM(data []byte) (interface{}, error) {
	block, data := pem.Decode(data)
	if block != nil {
		var rawKey interface{}
		var err error
		if rawKey, err = x509.ParsePKIXPublicKey(block.Bytes); err != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				rawKey = cert.PublicKey
			} else {
				return nil, err
			}
		}

		if rsaPublicKey, ok := rawKey.(*rsa.PublicKey); ok {
			return rsaPublicKey, nil
		}
		if ecPublicKey, ok := rawKey.(*ecdsa.PublicKey); ok {
			return ecPublicKey, nil
		}
	}

	return nil, errors.New("data does not contain any valid RSA or ECDSA public keys")
}

// ParseAndValidateJWT is used to parse, validate and lookup the JWT token.
func ParseAndValidateJWT(
	jwtStr string,
	publicKeys []interface{},
) (*ServiceAccount, error) {
	// Parse into JWT
	parsedJWT, err := jws.ParseJWT([]byte(jwtStr))
	if err != nil {
		return nil, err
	}

	sa := &ServiceAccount{}

	validator := &jwt.Validator{
		Expected: jwt.Claims{
			"iss": expectedJWTIssuer,
		},
		Fn: func(c jwt.Claims) error {
			// Decode claims into a service account object
			err := mapstructure.Decode(c, sa)
			if err != nil {
				return err
			}

			if sa.EffectiveNamespace() == "" {
				return fmt.Errorf("the service account has no namespace")
			} else if sa.EffectiveName() == "" {
				return fmt.Errorf("the service account has no name")
			} else if sa.EffectiveUID() == "" {
				return fmt.Errorf("the service account has no UID")
			}

			if sa.EffectiveName() == "default" {
				return fmt.Errorf("the service account named 'default' is not allowed to login")
			}

			return nil
		},
	}

	if err := validator.Validate(parsedJWT); err != nil {
		return nil, err
	}

	// If we don't have any public keys to verify, return the sa and end early.
	if len(publicKeys) == 0 {
		return sa, nil
	}

	// verifyFunc is called for each certificate that is configured in the
	// backend until one of the certificates succeeds.
	verifyFunc := func(cert interface{}) error {
		// Parse Headers and verify the signing method matches the public key type
		// configured. This is done in its own scope since we don't need most of
		// these variables later.
		var signingMethod crypto.SigningMethod
		{
			parsedJWS, err := jws.Parse([]byte(jwtStr))
			if err != nil {
				return err
			}
			headers := parsedJWS.Protected()

			var algStr string
			if headers.Has("alg") {
				algStr = headers.Get("alg").(string)
			} else {
				return errors.New("provided JWT must have 'alg' header value")
			}

			signingMethod = jws.GetSigningMethod(algStr)
			switch signingMethod.(type) {
			case *crypto.SigningMethodECDSA:
				if _, ok := cert.(*ecdsa.PublicKey); !ok {
					return errMismatchedSigningMethod
				}
			case *crypto.SigningMethodRSA:
				if _, ok := cert.(*rsa.PublicKey); !ok {
					return errMismatchedSigningMethod
				}
			default:
				return errors.New("unsupported JWT signing method")
			}
		}

		// validates the signature and then runs the claim validation
		if err := parsedJWT.Validate(cert, signingMethod); err != nil {
			return err
		}

		return nil
	}

	var validationErr error
	// for each configured certificate run the verifyFunc

	for _, cert := range publicKeys {
		err := verifyFunc(cert)
		switch err {
		case nil:
			return sa, nil
		case rsa.ErrVerification, crypto.ErrECDSAVerification, errMismatchedSigningMethod:
			// if the error is a failure to verify or a signing method mismatch
			// continue onto the next cert, storing the error to be returned if
			// this is the last cert.
			validationErr = multierror.Append(validationErr, errwrap.Wrapf("failed to validate JWT: {{err}}", err))
			continue
		default:
			return nil, err
		}
	}

	return nil, validationErr
}

// This is the result from the token review
type TokenReviewResult struct {
	Namespace    string
	Name         string
	UID          string
	OriginalName string
}

// This exists so we can use a mock TokenReview when running tests
type TokenReviewer interface {
	Review(string) (*TokenReviewResult, error)
}

// This is the real implementation that calls the kubernetes API
type TokenReviewAPI struct {
	idp      *structs.ACLIdentityProvider // config
	saGetter client_corev1.ServiceAccountsGetter
	trGetter client_authv1.TokenReviewsGetter
}

func NewTokenReviewer(idp *structs.ACLIdentityProvider) (*TokenReviewAPI, error) {
	transport := cleanhttp.DefaultTransport()
	k8sclient, err := k8s.NewForConfig(&client_rest.Config{
		Host:        idp.KubernetesHost,
		BearerToken: idp.KubernetesServiceAccountJWT,
		Dial:        transport.DialContext,
		TLSClientConfig: client_rest.TLSClientConfig{
			CAData: []byte(idp.KubernetesCACert),
		},
		ContentConfig: client_rest.ContentConfig{
			ContentType: "application/json",
		},
	})
	if err != nil {
		return nil, err
	}

	t := &TokenReviewAPI{
		idp:      idp,
		saGetter: k8sclient.CoreV1(),
		trGetter: k8sclient.AuthenticationV1(),
	}

	return t, nil
}

func (t *TokenReviewAPI) Review(jwt string) (*TokenReviewResult, error) {
	r, err := t.trGetter.TokenReviews().Create(&authv1.TokenReview{
		Spec: authv1.TokenReviewSpec{
			Token: jwt,
		},
	})
	if err != nil {
		return nil, err
	}

	if r.Status.Error != "" {
		// Error indicates that the token couldn't be checked
		return nil, fmt.Errorf("lookup failed: %s", r.Status.Error)
	} else if !r.Status.Authenticated {
		// Authenticated indicates that the token was associated with a known user.
		return nil, errors.New("lookup failed: service account jwt not valid")
	}

	// The username is of format: system:serviceaccount:(NAMESPACE):(SERVICEACCOUNT)
	parts := strings.Split(r.Status.User.Username, ":")
	if len(parts) != 4 {
		return nil, errors.New("lookup failed: unexpected username format")
	}

	// Validate the user that comes back from token review is a service account
	if parts[0] != "system" || parts[1] != "serviceaccount" {
		return nil, errors.New("lookup failed: username returned is not a service account")
	}

	tr := &TokenReviewResult{
		Namespace: parts[2],
		Name:      parts[3],
		UID:       string(r.Status.User.UID),
	}
	tr.OriginalName = tr.Name

	// check to see  if there is an override name
	sa, err := t.readServiceAccount(tr.Namespace, tr.Name, tr.UID)
	if err != nil {
		return nil, err
	}

	annotations := sa.GetObjectMeta().GetAnnotations()

	if serviceNameOverride, ok := annotations[serviceAccountServiceNameAnnotation]; ok {
		tr.Name = serviceNameOverride
	}

	return tr, nil
}

func (t *TokenReviewAPI) readServiceAccount(namespace, name, uid string) (*corev1.ServiceAccount, error) {
	saClient := t.saGetter.ServiceAccounts(namespace)
	return saClient.Get(name, client_metav1.GetOptions{})
}

// serviceAccount holds the metadata from the JWT token and is used to lookup
// the JWT in the kubernetes API and compare the results.
type ServiceAccount struct {
	Name       string   `mapstructure:"kubernetes.io/serviceaccount/service-account.name"`
	UID        string   `mapstructure:"kubernetes.io/serviceaccount/service-account.uid"`
	SecretName string   `mapstructure:"kubernetes.io/serviceaccount/secret.name"`
	Namespace  string   `mapstructure:"kubernetes.io/serviceaccount/namespace"`
	Aud        []string `mapstructure:"aud"`

	// the JSON returned from reviewing a Projected Service account has a
	// different structure, where the information is in a sub-structure instead of
	// at the top level
	Kubernetes *ProjectedServiceToken `mapstructure:"kubernetes.io"`
	Expiration int64                  `mapstructure:"exp"`
	IssuedAt   int64                  `mapstructure:"iat"`
}

// EffectiveUID returns the UID for the service account, preferring the projected service
// account value if found
func (s *ServiceAccount) EffectiveUID() string {
	if s.Kubernetes != nil && s.Kubernetes.ServiceAccount != nil {
		return s.Kubernetes.ServiceAccount.UID
	}
	return s.UID
}

// EffectiveName returns the name for the service account, preferring the projected
// service account value if found. This is "default" for projected service
// accounts
func (s *ServiceAccount) EffectiveName() string {
	if s.Kubernetes != nil && s.Kubernetes.ServiceAccount != nil {
		return s.Kubernetes.ServiceAccount.Name
	}
	return s.Name
}

// EffectiveNamespace returns the namespace for the service account, preferring the
// projected service account value if found
func (s *ServiceAccount) EffectiveNamespace() string {
	if s.Kubernetes != nil {
		return s.Kubernetes.Namespace
	}
	return s.Namespace
}

type ProjectedServiceToken struct {
	Namespace      string                      `mapstructure:"namespace"`
	Pod            *ProjectedServiceAccountPod `mapstructure:"pod"`
	ServiceAccount *ProjectedServiceAccountPod `mapstructure:"serviceaccount"`
}

type ProjectedServiceAccountPod struct {
	Name string `mapstructure:"name"`
	UID  string `mapstructure:"uid"`
}