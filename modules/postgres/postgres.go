package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/testcontainers/testcontainers-go"
)

const (
	defaultUser          = "postgres"
	defaultPassword      = "postgres"
	defaultPostgresImage = "docker.io/postgres:16-alpine"
	defaultSnapshotName  = "migrated_template"
)

// SQLDriverName is passed to sql.Open() to connect to the database when making or restoring snapshots.
// This can be set if your app imports a different postgres driver, f.ex. "pgx"
var SQLDriverName = "postgres"

// PostgresContainer represents the postgres container type used in the module
type PostgresContainer struct {
	testcontainers.Container
	dbName       string
	user         string
	password     string
	snapshotName string

	restoreConnection     *sql.DB
	restoreConnectionOnce sync.Once
}

// MustConnectionString panics if the address cannot be determined.
func (c *PostgresContainer) MustConnectionString(ctx context.Context, args ...string) string {
	addr, err := c.ConnectionString(ctx, args...)
	if err != nil {
		panic(err)
	}
	return addr
}

// ConnectionString returns the connection string for the postgres container, using the default 5432 port, and
// obtaining the host and exposed port from the container. It also accepts a variadic list of extra arguments
// which will be appended to the connection string. The format of the extra arguments is the same as the
// connection string format, e.g. "connect_timeout=10" or "application_name=myapp"
func (c *PostgresContainer) ConnectionString(ctx context.Context, args ...string) (string, error) {
	containerPort, err := c.MappedPort(ctx, "5432/tcp")
	if err != nil {
		return "", err
	}

	host, err := c.Host(ctx)
	if err != nil {
		return "", err
	}

	extraArgs := strings.Join(args, "&")
	connStr := fmt.Sprintf("postgres://%s:%s@%s/%s?%s", c.user, c.password, net.JoinHostPort(host, containerPort.Port()), c.dbName, extraArgs)
	return connStr, nil
}

// WithConfigFile sets the config file to be used for the postgres container
// It will also set the "config_file" parameter to the path of the config file
// as a command line argument to the container
func WithConfigFile(cfg string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		cfgFile := testcontainers.ContainerFile{
			HostFilePath:      cfg,
			ContainerFilePath: "/etc/postgresql.conf",
			FileMode:          0o755,
		}

		req.Files = append(req.Files, cfgFile)
		req.Cmd = append(req.Cmd, "-c", "config_file=/etc/postgresql.conf")

		return nil
	}
}

// WithDatabase sets the initial database to be created when the container starts
// It can be used to define a different name for the default database that is created when the image is first started.
// If it is not specified, then the value of WithUser will be used.
func WithDatabase(dbName string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		req.Env["POSTGRES_DB"] = dbName

		return nil
	}
}

// WithInitScripts sets the init scripts to be run when the container starts
func WithInitScripts(scripts ...string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		initScripts := []testcontainers.ContainerFile{}
		for _, script := range scripts {
			cf := testcontainers.ContainerFile{
				HostFilePath:      script,
				ContainerFilePath: "/docker-entrypoint-initdb.d/" + filepath.Base(script),
				FileMode:          0o755,
			}
			initScripts = append(initScripts, cf)
		}
		req.Files = append(req.Files, initScripts...)

		return nil
	}
}

// WithPassword sets the initial password of the user to be created when the container starts
// It is required for you to use the PostgreSQL image. It must not be empty or undefined.
// This environment variable sets the superuser password for PostgreSQL.
func WithPassword(password string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		req.Env["POSTGRES_PASSWORD"] = password

		return nil
	}
}

// WithUsername sets the initial username to be created when the container starts
// It is used in conjunction with WithPassword to set a user and its password.
// It will create the specified user with superuser power and a database with the same name.
// If it is not specified, then the default user of postgres will be used.
func WithUsername(user string) testcontainers.CustomizeRequestOption {
	return func(req *testcontainers.GenericContainerRequest) error {
		if user == "" {
			user = defaultUser
		}

		req.Env["POSTGRES_USER"] = user

		return nil
	}
}

// RunContainer creates an instance of the postgres container type
func RunContainer(ctx context.Context, opts ...testcontainers.ContainerCustomizer) (*PostgresContainer, error) {
	req := testcontainers.ContainerRequest{
		Image: defaultPostgresImage,
		Env: map[string]string{
			"POSTGRES_USER":     defaultUser,
			"POSTGRES_PASSWORD": defaultPassword,
			"POSTGRES_DB":       defaultUser, // defaults to the user name
		},
		ExposedPorts: []string{"5432/tcp"},
		Cmd:          []string{"postgres", "-c", "fsync=off"},
	}

	genericContainerReq := testcontainers.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	}

	for _, opt := range opts {
		if err := opt.Customize(&genericContainerReq); err != nil {
			return nil, err
		}
	}

	container, err := testcontainers.GenericContainer(ctx, genericContainerReq)
	if err != nil {
		return nil, err
	}

	user := req.Env["POSTGRES_USER"]
	password := req.Env["POSTGRES_PASSWORD"]
	dbName := req.Env["POSTGRES_DB"]

	return &PostgresContainer{Container: container, dbName: dbName, password: password, user: user}, nil
}

// Stop overrides testcontainers.Container.Stop() to close the database connection before stopping the container
func (c *PostgresContainer) Stop(ctx context.Context, timeout *time.Duration) error {
	c.closeRestoreConnection()
	return c.Container.Stop(ctx, timeout)
}

// Terminate overrides testcontainers.Container.Terminate() to close the database connection before terminate the container
func (c *PostgresContainer) Terminate(ctx context.Context) error {
	c.closeRestoreConnection()
	return c.Container.Terminate(ctx)
}

func (c *PostgresContainer) closeRestoreConnection() {
	if c.restoreConnection != nil {
		if err := c.restoreConnection.Close(); err != nil {
			testcontainers.Logger.Printf("Could not close database connection before stopping container: %v", err)
		}
		c.restoreConnection = nil
	}
}

type snapshotConfig struct {
	snapshotName string
}

// SnapshotOption is the type for passing options to the snapshot function of the database
type SnapshotOption func(container *snapshotConfig) *snapshotConfig

// WithSnapshotName adds a specific name to the snapshot database created from the main database defined on the
// container. The snapshot must not have the same name as your main database, otherwise it will be overwritten
func WithSnapshotName(name string) SnapshotOption {
	return func(cfg *snapshotConfig) *snapshotConfig {
		cfg.snapshotName = name
		return cfg
	}
}

// Snapshot takes a snapshot of the current state of the database as a template, which can then be restored using
// the Restore method. By default, the snapshot will be created under a database called migrated_template, you can
// customize the snapshot name with the options.
// If a snapshot already exists under the given/default name, it will be overwritten with the new snapshot.
func (c *PostgresContainer) Snapshot(ctx context.Context, opts ...SnapshotOption) error {
	snapshotName, err := c.checkSnapshotConfig(opts)
	if err != nil {
		return err
	}

	// execute the commands to create the snapshot, in order
	if err := c.execCommandsSQL(ctx,
		// Drop the snapshot database if it already exists
		fmt.Sprintf(`DROP DATABASE IF EXISTS "%s"`, snapshotName),
		// Create a copy of the database to another database to use as a template now that it was fully migrated
		fmt.Sprintf(`CREATE DATABASE "%s" WITH TEMPLATE "%s" OWNER "%s"`, snapshotName, c.dbName, c.user),
		// Snapshot the template database so we can restore it onto our original database going forward
		fmt.Sprintf(`ALTER DATABASE "%s" WITH is_template = TRUE`, snapshotName),
	); err != nil {
		return err
	}

	c.snapshotName = snapshotName
	return nil
}

// Restore will restore the database to a specific snapshot. By default, it will restore the last snapshot taken on the
// database by the Snapshot method. If a snapshot name is provided, it will instead try to restore the snapshot by name.
func (c *PostgresContainer) Restore(ctx context.Context, opts ...SnapshotOption) error {
	snapshotName, err := c.checkSnapshotConfig(opts)
	if err != nil {
		return err
	}

	// execute the commands to restore the snapshot, in order
	return c.execCommandsSQL(ctx,
		// Drop the entire database by connecting to the postgres global database
		fmt.Sprintf(`DROP DATABASE "%s" with (FORCE)`, c.dbName),
		// Then restore the previous snapshot
		fmt.Sprintf(`CREATE DATABASE "%s" WITH TEMPLATE "%s" OWNER "%s"`, c.dbName, snapshotName, c.user),
	)
}

func (c *PostgresContainer) checkSnapshotConfig(opts []SnapshotOption) (string, error) {
	config := &snapshotConfig{}
	for _, opt := range opts {
		config = opt(config)
	}

	snapshotName := c.snapshotName
	if config.snapshotName != "" {
		snapshotName = config.snapshotName
	}

	if c.dbName == "postgres" {
		return "", fmt.Errorf("cannot restore the postgres system database as it cannot be dropped to be restored")
	}
	return snapshotName, nil
}

func (c *PostgresContainer) execCommandsSQL(ctx context.Context, cmds ...string) error {
	conn, err := c.snapshotConnection(ctx)
	if err != nil {
		testcontainers.Logger.Printf("Could not connect to database to restore snapshot, falling back to `docker exec psql`: %v", err)
		return c.execCommandsFallback(ctx, cmds)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			testcontainers.Logger.Printf("Could not close database connection after restore snapshot: %v", err)
		}
	}()
	for _, cmd := range cmds {
		if _, err := conn.ExecContext(ctx, cmd); err != nil {
			return fmt.Errorf("could not execute restore command %s: %w", cmd, err)
		}
	}
	return nil
}

// snapshotConnection connects to the actual database using the "postgres" sql.DB driver, if it exists.
// The returned function should be called as a defer() to close the pool.
// No need to close the individual connection, that is done as part of the pool close.
// Also, no need to cache the connection pool, since it is a single connection which is very fast to establish.
func (c *PostgresContainer) snapshotConnection(ctx context.Context) (*sql.Conn, error) {
	var err error
	c.restoreConnectionOnce.Do(func() {
		// Connect to the database "postgres" instead of the app one
		c2 := &PostgresContainer{
			Container: c.Container,
			dbName:    "postgres",
			user:      c.user,
			password:  c.password,
		}

		// Try to use an actual postgres connection, if the driver is loaded
		connStr := c2.MustConnectionString(ctx, "sslmode=disable")
		var pool *sql.DB
		pool, err = sql.Open(SQLDriverName, connStr)
		if err == nil {
			c.restoreConnection = pool
		}
	})
	if err != nil {
		// Reset the sync.Once so we can try again next time
		c.restoreConnectionOnce = sync.Once{}
		return nil, fmt.Errorf("sql.Open for snapshot connection failed: %w", err)
	}

	conn, err := c.restoreConnection.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("DB.Conn for snapshot connection failed: %w", err)
	}
	return conn, nil
}

func (c *PostgresContainer) execCommandsFallback(ctx context.Context, cmds []string) error {
	for _, cmd := range cmds {
		exitCode, reader, err := c.Exec(ctx, []string{"psql", "-v", "ON_ERROR_STOP=1", "-U", c.user, "-d", "postgres", "-c", cmd})
		if err != nil {
			return err
		}
		if exitCode != 0 {
			buf := new(strings.Builder)
			_, err := io.Copy(buf, reader)
			if err != nil {
				return fmt.Errorf("non-zero exit code for restore command, could not read command output: %w", err)
			}

			return fmt.Errorf("non-zero exit code for restore command: %s", buf.String())
		}
	}
	return nil
}
