package migratest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gobuffalo/pop/v5"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ory/kratos/driver"
	"github.com/ory/kratos/driver/config"
	"github.com/ory/kratos/internal/testhelpers"
	"github.com/ory/kratos/selfservice/flow/login"
	"github.com/ory/kratos/selfservice/flow/recovery"
	"github.com/ory/kratos/selfservice/flow/registration"
	"github.com/ory/kratos/selfservice/flow/settings"
	"github.com/ory/kratos/selfservice/flow/verification"
	"github.com/ory/kratos/selfservice/strategy/link"
	"github.com/ory/kratos/session"
	"github.com/ory/kratos/x"
	"github.com/ory/x/configx"
	"github.com/ory/x/logrusx"
	"github.com/ory/x/popx"
	"github.com/ory/x/sqlcon"
	"github.com/ory/x/sqlcon/dockertest"
)

func containsExpectedIds(t *testing.T, path string, ids []string) {
	files, err := ioutil.ReadDir(path)
	require.NoError(t, err)

	for _, f := range files {
		if filepath.Ext(f.Name()) == ".json" {
			expected := strings.TrimSuffix(filepath.Base(f.Name()), ".json")
			assert.Contains(t, ids, expected)
		}
	}
}

func TestMigrations(t *testing.T) {
	sqlite, err := pop.NewConnection(&pop.ConnectionDetails{
		URL: "sqlite3://" + filepath.Join(os.TempDir(), x.NewUUID().String()) + ".sql?_fk=true",
	})
	require.NoError(t, err)
	require.NoError(t, sqlite.Open())

	connections := map[string]*pop.Connection{
		"sqlite": sqlite,
	}

	if !testing.Short() {
		dockertest.Parallel([]func(){
			func() {
				connections["postgres"] = dockertest.ConnectToTestPostgreSQLPop(t)
			},
			func() {
				connections["mysql"] = dockertest.ConnectToTestMySQLPop(t)
			},
			func() {
				connections["cockroach"] = dockertest.ConnectToTestCockroachDBPop(t)
			},
		})
	}

	var test = func(db string, c *pop.Connection) func(t *testing.T) {
		return func(t *testing.T) {
			defer func() {
				if err := recover(); err != nil {
					t.Fatalf("recovered: %+v\n\t%s", err, debug.Stack())
				}
			}()

			ctx := context.Background()
			l := logrusx.New("", "", logrusx.ForceLevel(logrus.DebugLevel))

			t.Logf("Cleaning up before migrations")
			_ = os.Remove("../migrations/sql/schema.sql")
			testhelpers.CleanSQL(t, c)

			t.Cleanup(func() {
				t.Logf("Cleaning up after migrations")
				testhelpers.CleanSQL(t, c)
				require.NoError(t, c.Close())
			})

			url := c.URL()
			// workaround for https://github.com/gobuffalo/pop/issues/538
			switch db {
			case "mysql":
				url = "mysql://" + url
			case "sqlite":
				url = "sqlite3://" + url
			}
			t.Logf("URL: %s", url)

			t.Run("suite=up", func(t *testing.T) {
				tm := popx.NewTestMigrator(t, c, "../migrations/sql", "./testdata", l)
				require.NoError(t, tm.Up(ctx))
			})

			t.Run("suite=fixtures", func(t *testing.T) {
				d := driver.New(
					context.Background(),
					configx.WithValues(map[string]interface{}{
						config.ViperKeyDSN:                      url,
						config.ViperKeyPublicBaseURL:            "https://www.ory.sh/",
						config.ViperKeyDefaultIdentitySchemaURL: "file://stub/default.schema.json",
						config.ViperKeySecretsDefault:           []string{"secret"},
					}),
					configx.SkipValidation(),
				)

				t.Run("case=identity", func(t *testing.T) {
					ids, err := d.PrivilegedIdentityPool().ListIdentities(context.Background(), 0, 1000)
					require.NoError(t, err)
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.PrivilegedIdentityPool().GetIdentityConfidential(context.Background(), id.ID)
						require.NoError(t, err)

						for _, a := range actual.VerifiableAddresses {
							compareWithFixture(t, a, "identity_verification_address", a.ID.String())
						}

						for _, a := range actual.RecoveryAddresses {
							compareWithFixture(t, a, "identity_recovery_address", a.ID.String())
						}

						// Prevents ordering to get in the way.
						actual.VerifiableAddresses = nil
						actual.RecoveryAddresses = nil
						compareWithFixture(t, actual, "identity", id.ID.String())
					}

					containsExpectedIds(t, filepath.Join("fixtures", "identity"), found)
				})

				t.Run("case=verification_token", func(t *testing.T) {
					var ids []link.VerificationToken

					require.NoError(t, c.All(&ids))
					require.NotEmpty(t, ids)

					for _, id := range ids {
						compareWithFixture(t, id, "verification_token", id.ID.String())
					}
				})

				t.Run("case=session", func(t *testing.T) {
					var ids []session.Session
					require.NoError(t, c.Select("id").All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.SessionPersister().GetSession(context.Background(), id.ID)
						require.NoError(t, err)
						compareWithFixture(t, actual, "session", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "session"), found)
				})

				t.Run("case=login", func(t *testing.T) {
					var ids []login.Flow
					require.NoError(t, c.Select("id").All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.LoginFlowPersister().GetLoginFlow(context.Background(), id.ID)
						require.NoError(t, err)
						compareWithFixture(t, actual, "login_flow", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "login_flow"), found)
				})

				t.Run("case=registration", func(t *testing.T) {
					var ids []registration.Flow
					require.NoError(t, c.Select("id").All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.RegistrationFlowPersister().GetRegistrationFlow(context.Background(), id.ID)
						require.NoError(t, err)
						compareWithFixture(t, actual, "registration_flow", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "registration_flow"), found)
				})

				t.Run("case=settings_flow", func(t *testing.T) {
					var ids []settings.Flow
					require.NoError(t, c.Select("id").All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.SettingsFlowPersister().GetSettingsFlow(context.Background(), id.ID)
						require.NoError(t, err)
						compareWithFixture(t, actual, "settings_flow", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "settings_flow"), found)
				})

				t.Run("case=recovery_flow", func(t *testing.T) {
					var ids []recovery.Flow
					require.NoError(t, c.Select("id").All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.RecoveryFlowPersister().GetRecoveryFlow(context.Background(), id.ID)
						require.NoError(t, err)
						compareWithFixture(t, actual, "recovery_flow", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "recovery_flow"), found)
				})

				t.Run("case=verification_flow", func(t *testing.T) {
					var ids []verification.Flow
					require.NoError(t, c.Select("id").All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						actual, err := d.VerificationFlowPersister().GetVerificationFlow(context.Background(), id.ID)
						require.NoError(t, err)
						compareWithFixture(t, actual, "verification_flow", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "verification_flow"), found)
				})

				t.Run("case=recovery_token", func(t *testing.T) {
					var ids []link.RecoveryToken
					require.NoError(t, c.All(&ids))
					require.NotEmpty(t, ids)

					var found []string
					for _, id := range ids {
						found = append(found, id.ID.String())
						compareWithFixture(t, id, "recovery_token", id.ID.String())
					}
					containsExpectedIds(t, filepath.Join("fixtures", "recovery_token"), found)
				})

				t.Run("suite=constraints", func(t *testing.T) {
					sr, err := d.SettingsFlowPersister().GetSettingsFlow(context.Background(), x.ParseUUID("a79bfcf1-68ae-49de-8b23-4f96921b8341"))
					require.NoError(t, err)

					require.NoError(t, d.PrivilegedIdentityPool().DeleteIdentity(context.Background(), sr.IdentityID))

					_, err = d.SettingsFlowPersister().GetSettingsFlow(context.Background(), x.ParseUUID("a79bfcf1-68ae-49de-8b23-4f96921b8341"))
					require.Error(t, err)
					require.True(t, errors.Is(err, sqlcon.ErrNoRows))
				})
			})

			t.Run("suite=down", func(t *testing.T) {
				tm := popx.NewTestMigrator(t, c, "../migrations/sql", "./testdata", l)
				require.NoError(t, tm.Down(ctx, -1))
			})
		}
	}

	for db, c := range connections {
		t.Run(fmt.Sprintf("database=%s", db), test(db, c))
	}
}

func compareWithFixture(t *testing.T, actual interface{}, prefix string, id string) {
	location := filepath.Join("fixtures", prefix, id+".json")
	expected, err := ioutil.ReadFile(location)
	writeFixtureOnError(t, err, actual, location)

	actualJSON, err := json.Marshal(actual)
	require.NoError(t, err)

	if !assert.JSONEq(t, string(expected), string(actualJSON)) {
		writeFixtureOnError(t, nil, actual, location)
	}
}
