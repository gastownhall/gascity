package storebinding

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestBindingSpecRejectsAmbiguousOrNonCanonicalConfigurationReferences(t *testing.T) {
	const secret = "hunter2" // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.

	tests := []struct {
		name string
		spec BindingSpec
	}{
		{
			name: "path and config reference",
			spec: BindingSpec{
				Name:      BindingName("ambiguous"),
				Provider:  ProviderID("builtin-test"),
				Path:      "file:///city/store.sqlite",
				ConfigRef: ConfigRef("city-store"),
			},
		},
		{
			name: "nested JSON credential",
			spec: BindingSpec{
				Name:      BindingName("nested-json"),
				Provider:  ProviderID("builtin-test"),
				ConfigRef: ConfigRef(`{"dsn":"postgres://user:` + secret + `@db.example.test/city"}`),
			},
		},
		{
			name: "credential colon value",
			spec: BindingSpec{
				Name:      BindingName("credential-colon"),
				Provider:  ProviderID("builtin-test"),
				ConfigRef: ConfigRef("password:" + secret),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.spec.Validate()
			if !errors.Is(err, ErrInvalidBindingSpec) {
				t.Fatalf("BindingSpec.Validate() error = %v, want ErrInvalidBindingSpec", err)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), string(test.spec.ConfigRef)) {
				t.Fatalf("BindingSpec.Validate() leaked configuration input: %q", err)
			}
		})
	}

	if err := (BindingSpec{Name: BindingName("default"), Provider: ProviderID("builtin-test")}).Validate(); err != nil {
		t.Fatalf("BindingSpec.Validate() rejected a provider-default envelope: %v", err)
	}
	if err := (BindingSpec{Name: BindingName("ref"), Provider: ProviderID("builtin-test"), ConfigRef: ConfigRef("city-store")}).Validate(); err != nil {
		t.Fatalf("BindingSpec.Validate() rejected a canonical config reference: %v", err)
	}
}

func TestPersistedProviderFieldsRejectSecretMaterialWithoutEchoingIt(t *testing.T) {
	const secret = "hunter2" // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.

	for _, test := range []struct {
		name     string
		validate func() error
	}{
		{
			name: "descriptor semantic contract",
			validate: func() error {
				descriptor := testDescriptor(t, ProviderID("builtin-hardening"), PhysicalIdentity("hardening-descriptor"), coordclass.ClassGraph)
				descriptor.SemanticContractVersion = ContractVersion("token=" + secret)
				return descriptor.Validate()
			},
		},
		{
			name: "component schema version",
			validate: func() error {
				descriptor := testDescriptor(t, ProviderID("builtin-hardening"), PhysicalIdentity("hardening-schema"), coordclass.ClassGraph)
				descriptor.Components[0].SchemaVersion = "token=" + secret
				return descriptor.Validate()
			},
		},
		{
			name: "component ABI version",
			validate: func() error {
				descriptor := testDescriptor(t, ProviderID("builtin-hardening"), PhysicalIdentity("hardening-abi"), coordclass.ClassGraph)
				descriptor.Components[0].ABIVersion = "token=" + secret
				return descriptor.Validate()
			},
		},
		{
			name: "component locator encoded path credential",
			validate: func() error {
				descriptor := testDescriptor(t, ProviderID("builtin-hardening"), PhysicalIdentity("hardening-locator"), coordclass.ClassGraph)
				descriptor.Components[0].Locator = ComponentLocator("file:///tmp/password%3D" + secret)
				return descriptor.Validate()
			},
		},
		{
			name: "retained schema version",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-schema"))
				source.SchemaVersion = "token=" + secret
				return source.Validate()
			},
		},
		{
			name: "retained ABI version",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-abi"))
				source.ABIVersion = "token=" + secret
				return source.Validate()
			},
		},
		{
			name: "retained witness version",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-witness"))
				source.WitnessVersion = "token=" + secret
				return source.Validate()
			},
		},
		{
			name: "retained reopen bytes",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-reopen"))
				source.ReopenData = []byte("token=" + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen credential colon",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-reopen-colon"))
				source.ReopenData = []byte("password: " + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen JSON DSN",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-reopen-json"))
				source.ReopenData = []byte(`{"dsn":"postgres://user:` + secret + `@db.example.test/city"}`)
				return source.Validate()
			},
		},
		{
			name: "retained reopen serialized JSON credential",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-reopen-serialized-json"))
				source.ReopenData = []byte(`{"payload":"{\"password\":\"` + secret + `\"}"}`)
				return source.Validate()
			},
		},
		{
			name: "retained reopen client secret assignment",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-client-secret"))
				source.ReopenData = []byte("client_secret=" + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen secret key assignment",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-secret-key"))
				source.ReopenData = []byte("secret_key: " + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen private key assignment",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-private-key"))
				source.ReopenData = []byte("private_key=" + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen authorization assignment",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-authorization"))
				source.ReopenData = []byte("authorization: Bearer " + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen cloud secret JSON key",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-cloud-secret"))
				source.ReopenData = []byte(`{"aws_secret_access_key":"` + secret + `"}`) // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
				return source.Validate()
			},
		},
		{
			name: "retained reopen encoded query credential",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-query-secret"))
				source.ReopenData = []byte("postgres://db.example.test/city?options=password%253D" + secret)
				return source.Validate()
			},
		},
		{
			name: "retained reopen private key PEM",
			validate: func() error {
				source := testRetainedSource(PhysicalIdentity("hardening-retained-pem"))
				source.ReopenData = []byte("-----BEGIN PRIVATE KEY-----\n" + secret + "\n-----END PRIVATE KEY-----") // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
				return source.Validate()
			},
		},
		{
			name: "guard revalidation JSON DSN",
			validate: func() error {
				request := GuardInstallRequest{
					Attempt:          AttemptID("hardening-guard-json"),
					Generation:       Generation(1),
					Source:           testRetainedSource(PhysicalIdentity("hardening-guard-json")),
					Component:        ComponentID("component"),
					PhysicalIdentity: PhysicalIdentity("hardening-guard-json"),
					Role:             GuardRoleDenyWrite,
				}
				receipt := matchingGuardReceipt(request)
				receipt.Revalidation = `{"endpoint":"postgres://user:` + secret + `@db.example.test/city"}`
				return receipt.Validate()
			},
		},
		{
			name: "work prepare witness version",
			validate: func() error {
				fixture := newWorkMigrationFixture(t)
				fixture.prepare.WitnessVersion = "token=" + secret
				return fixture.prepare.Validate(context.Background())
			},
		},
		{
			name: "work preparation witness version",
			validate: func() error {
				fixture := newWorkMigrationFixture(t)
				preparation := fixture.preparation.Clone()
				preparation.WitnessVersion = "token=" + secret
				return preparation.Validate()
			},
		},
		{
			name: "commit decision attempt",
			validate: func() error {
				return CommitDecision{Attempt: AttemptID("token=" + secret), Generation: Generation(1), Decided: true}.Validate()
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.validate()
			if !errors.Is(err, ErrSecretMaterial) {
				t.Fatalf("Validate() error = %v, want ErrSecretMaterial", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("Validate() leaked secret: %q", err)
			}
		})
	}
}

func TestProviderValidationRejectsInvalidProviderAndComponentIdentifiers(t *testing.T) {
	member := workMember(HQScope(), "hq", 0, false, "identifier-workspace")
	member.Component = ComponentID("invalid/component")
	if err := member.Validate(); !errors.Is(err, ErrInvalidWorkParticipant) {
		t.Fatalf("WorkWorkspaceMember.Validate() error = %v, want ErrInvalidWorkParticipant", err)
	}

	request := GuardInstallRequest{
		Attempt:          AttemptID("identifier-guard"),
		Generation:       Generation(3),
		Source:           testRetainedSource(PhysicalIdentity("identifier-guard")),
		Component:        ComponentID("component"),
		PhysicalIdentity: PhysicalIdentity("identifier-guard"),
		Role:             GuardRoleDenyWrite,
	}
	receipt := matchingGuardReceipt(request)
	receipt.Component = ComponentID("invalid/component")
	if err := receipt.Validate(); !errors.Is(err, ErrInvalidGuard) {
		t.Fatalf("GuardReceipt.Validate() error = %v, want ErrInvalidGuard", err)
	}
}

func TestSecretAndCGOErrorsDoNotEchoUntrustedConstructionFields(t *testing.T) {
	const secret = "hunter2"                                                                                                                       // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
	if err := validateSecretFree("field-"+secret, "token=redacted"); !errors.Is(err, ErrSecretMaterial) || strings.Contains(err.Error(), secret) { // oss-leak-allow:fixture — negative-path input proving the validator rejects credential-shaped material.
		t.Fatalf("validateSecretFree() error = %v, want redacted ErrSecretMaterial", err)
	}

	cgo := NewCGOUnavailableError(ProviderID("provider-"+secret), "open-"+secret)
	if !errors.Is(cgo, ErrCGOUnavailable) || !errors.Is(cgo, ErrProviderUnavailable) {
		t.Fatalf("CGOUnavailableError = %v, want typed unavailable causes", cgo)
	}
	if strings.Contains(cgo.Error(), secret) {
		t.Fatalf("CGOUnavailableError leaked construction field: %q", cgo)
	}
}
