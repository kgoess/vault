package database

import (
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/go-uuid"
	"github.com/hashicorp/vault/helper/strutil"
	"github.com/hashicorp/vault/logical"
	"github.com/hashicorp/vault/logical/framework"
	_ "github.com/lib/pq"
)

func pathRoleCreate(b *backend) *framework.Path {
	return &framework.Path{
		Pattern: "creds/" + framework.GenericNameRegex("name"),
		Fields: map[string]*framework.FieldSchema{
			"name": &framework.FieldSchema{
				Type:        framework.TypeString,
				Description: "Name of the role.",
			},
		},

		Callbacks: map[logical.Operation]framework.OperationFunc{
			logical.ReadOperation: b.pathRoleCreateRead,
		},

		HelpSynopsis:    pathRoleCreateReadHelpSyn,
		HelpDescription: pathRoleCreateReadHelpDesc,
	}
}

func (b *backend) pathRoleCreateRead(
	req *logical.Request, data *framework.FieldData) (*logical.Response, error) {
	b.logger.Trace("[TRACE] db/pathRoleCreateRead: enter")
	defer b.logger.Trace("[TRACE] db/pathRoleCreateRead: exit")
	name := data.Get("name").(string)
	db_name := data.Get("db_name").(string)

	// Get the role
	b.logger.Trace("[TRACE] db/pathRoleCreateRead: getting role")
	role, err := b.Role(req.Storage, name)
	if err != nil {
		return nil, err
	}
	if role == nil {
		return logical.ErrorResponse(fmt.Sprintf("unknown role: %s", name)), nil
	}

	// Determine if we have a lease
	b.logger.Trace("[TRACE] db/pathRoleCreateRead: getting lease")
	lease, err := b.Lease(req.Storage)
	if err != nil {
		return nil, err
	}
	// Unlike some other backends we need a lease here (can't leave as 0 and
	// let core fill it in) because Postgres also expires users as a safety
	// measure, so cannot be zero
	if lease == nil {
		lease = &configLease{
			Lease: b.System().DefaultLeaseTTL(),
		}
	}

	// Generate the username, password and expiration. PG limits user to 63 characters
	displayName := req.DisplayName
	if len(displayName) > 26 {
		displayName = displayName[:26]
	}
	userUUID, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}
	username := fmt.Sprintf("%s-%s", displayName, userUUID)
	if len(username) > 63 {
		username = username[:63]
	}
	password, err := uuid.GenerateUUID()
	if err != nil {
		return nil, err
	}
	expiration := time.Now().
		Add(lease.Lease).
		Format("2006-01-02 15:04:05-0700")

	// Start a transaction
	b.logger.Trace("[TRACE] db/pathRoleCreateRead: starting transaction")
	
	if b.dbs[db_name] == nil {
		b.logger.Trace("[TRACE] b.dbs[%s] is not connected.", db_name)
		// Get our connection
		err = b.DBConnection(req.Storage, db_name)
		
		return nil, err
	}
	
	b.logger.Trace("[TRACE] b.dbs[%s] starting transaction.", db_name)
	tx, err := b.dbs[db_name].Begin()
	if err != nil {
		return nil, err
	}
	defer func() {
		b.logger.Trace("[TRACE] db/pathRoleCreateRead: rolling back transaction")
		tx.Rollback()
	}()

	// Execute each query
	for _, query := range strutil.ParseArbitraryStringSlice(role.SQL, ";") {
		query = strings.TrimSpace(query)
		if len(query) == 0 {
			continue
		}

		b.logger.Trace("[TRACE] db/pathRoleCreateRead: preparing statement")
		stmt, err := tx.Prepare(Query(query, map[string]string{
			"name":       username,
			"password":   password,
			"expiration": expiration,
		}))
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		b.logger.Trace("[TRACE] db/pathRoleCreateRead: executing statement")
		if _, err := stmt.Exec(); err != nil {
			return nil, err
		}
	}

	// Commit the transaction

	b.logger.Trace("[TRACE] db/pathRoleCreateRead: committing transaction")
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// Return the secret

	b.logger.Trace("[TRACE] db/pathRoleCreateRead: generating secret")
	resp := b.Secret(SecretCredsType).Response(map[string]interface{}{
		"username": username,
		"password": password,
	}, map[string]interface{}{
		"username": username,
	})
	resp.Secret.TTL = lease.Lease
	return resp, nil
}

const pathRoleCreateReadHelpSyn = `
Request database credentials for a certain role.
`

const pathRoleCreateReadHelpDesc = `
This path reads database credentials for a certain role. The
database credentials will be generated on demand and will be automatically
revoked when the lease is up.
`