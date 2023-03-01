// Package resources contains objects that are used to gather information about Kusto resources that are
// used during various ingestion methods.
package resources

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-kusto-go/kusto"
	"github.com/Azure/azure-kusto-go/kusto/data/table"
)

// mgmter is a private interface that allows us to write hermetic tests against the kusto.Client.Mgmt() method.
type mgmter interface {
	Mgmt(ctx context.Context, db string, query kusto.Stmt, options ...kusto.MgmtOption) (*kusto.RowIterator, error)
}

var objectTypes = map[string]bool{
	"queue": true,
	"blob":  true,
	"table": true,
}

// URI represents a resource URI for an ingestion command.
type URI struct {
	u                               *url.URL
	account, objectType, objectName string
	sas                             url.Values
}

// parse parses a string representing a Kutso resource URI.
func parse(uri string) (*URI, error) {
	// Regex representing URI that is expected:
	// https://(\w+).(queue|blob|table).core.windows.net/([\w,-]+)\?(.*)

	u, err := url.Parse(uri)
	if err != nil {
		return nil, err
	}

	if u.Scheme != "https" {
		return nil, fmt.Errorf("URI scheme must be 'https', was '%s'", u.Scheme)
	}

	if !strings.HasSuffix(u.Hostname(), ".core.windows.net") {
		return nil, fmt.Errorf("URI hostname does not end with '.core.windows.net'")
	}

	hostSplit := strings.Split(u.Hostname(), ".")
	if len(hostSplit) != 5 {
		return nil, fmt.Errorf("URI(%s) is invalid: had incorrect URL path before '.core.windows.net'", uri)
	}

	v := &URI{
		u:          u,
		account:    hostSplit[0],
		objectType: hostSplit[1],
		objectName: strings.TrimLeft(u.EscapedPath(), "/"),
		sas:        u.Query(),
	}

	if err := v.validate(); err != nil {
		return nil, err
	}

	return v, nil
}

// validate validates that the URI was valid.
// TODO(Daniel): You could add deep validation of each value we have split to give better diagnostic info on an error.
// I put in the most basic evalutation, but you might want to put checks for the account format or objectName foramt.
func (u *URI) validate() error {
	if u.account == "" {
		return fmt.Errorf("account name was not provided")
	}
	if !objectTypes[u.objectType] {
		return fmt.Errorf("object type was not valid(queue|blob|table), was: %q", u.objectType)
	}
	if u.objectName == "" {
		return fmt.Errorf("object name was not provided")
	}

	return nil
}

// Account is the Azure storage account that will be used.
func (u *URI) Account() string {
	return u.account
}

// ObjectType returns the type of object that will be ingested: queue, blob or table.
func (u *URI) ObjectType() string {
	return u.objectType
}

// ObjectName returns the object name that will be ingested???
func (u *URI) ObjectName() string {
	return u.objectName
}

// SAS is shared access signature used to access Azure storage.
// https://docs.microsoft.com/en-us/azure/storage/common/storage-sas-overview
func (u *URI) SAS() url.Values {
	return u.sas
}

// String implements fmt.Stringer.
func (u *URI) String() string {
	return u.u.String()
}

// URL returns the internal *url.URL object.
func (u *URI) URL() *url.URL {
	return u.u
}

// token represents a Kusto identity token.
type token struct {
	AuthContext string `kusto:"AuthorizationContext"`
}

// Manager manages Kusto resources.
type Manager struct {
	client                    mgmter
	done                      chan struct{}
	resources                 atomic.Value // Stores Ingestion
	kustoToken                token
	kustoTokenCacheExpiration time.Time
}

// New is the constructor for Manager.
func New(client mgmter) (*Manager, error) {
	m := &Manager{client: client, done: make(chan struct{})}
	if err := m.fetch(context.Background()); err != nil {
		return nil, err
	}

	m.kustoTokenCacheExpiration = time.Now().UTC()
	go m.renewResources()

	return m, nil
}

// Close closes the manager. This stops any token refreshes.
func (m *Manager) Close() {
	close(m.done)
}

func (m *Manager) renewResources() {
	tick := time.NewTicker(1 * time.Hour)
	for {
		select {
		case <-tick.C:
			m.fetchRetry(context.Background())
		case <-m.done:
			tick.Stop()
			return
		}
	}
}

// AuthContext returns a string representing the authorization context. This auth token is a temporary token
// that can be used to write a message via ingestion.  This is different than the ADAL token.
func (m *Manager) AuthContext(ctx context.Context) (string, error) {
	if m.kustoTokenCacheExpiration.After(time.Now().UTC()) {
		return m.kustoToken.AuthContext, nil
	}

	rows, err := m.client.Mgmt(ctx, "NetDefaultDB", kusto.NewStmt(".get kusto identity token"), kusto.IngestionEndpoint())
	if err != nil {
		return "", fmt.Errorf("problem getting authorization context from Kusto via Mgmt: %s", err)
	}

	count := 0
	token := token{}
	err = rows.Do(
		func(r *table.Row) error {
			if count != 0 {
				return fmt.Errorf("call for AuthContext returned more than 1 Row")
			}
			count++
			return r.ToStruct(&token)
		},
	)
	if err != nil {
		return "", err
	}

	m.kustoToken = token
	m.kustoTokenCacheExpiration = time.Now().UTC().Add(time.Hour)
	return token.AuthContext, nil
}

// ingestResc represents a kusto Mgmt() record about a resource
type ingestResc struct {
	// Type is the type of resource, either "TempStorage" or "SecuredReadyForAggregationQueue".
	Type string `kusto:"ResourceTypeName"`
	// Root is the storage root URI, which should conform to the local URI type.
	Root string `kusto:"StorageRoot"`
}

// Ingestion holds information about Ingestion resources.
type Ingestion struct {
	// Queues contains URIs for Queue resources.
	Queues []*URI
	// Containers has URIs for blob resources.
	Containers []*URI
	// Tables contains URIs for table resources.
	Tables []*URI
}

var errDoNotCare = errors.New("don't care about this")

func (i *Ingestion) importRec(rec ingestResc) error {
	u, err := parse(rec.Root)
	if err != nil {
		return fmt.Errorf("the StorageRoot URI received(%s) has an error: %s", rec.Root, err)
	}

	switch rec.Type {
	case "TempStorage":
		i.Containers = append(i.Containers, u)
	case "SecuredReadyForAggregationQueue":
		i.Queues = append(i.Queues, u)
	case "IngestionsStatusTable":
		i.Tables = append(i.Tables, u)
	default:
		return errDoNotCare
	}
	return nil
}

// fetch makes a kusto.Client.Mgmt() call to retrieve the resources used for Ingestion.
func (m *Manager) fetch(ctx context.Context) error {
	rows, err := m.client.Mgmt(ctx, "NetDefaultDB", kusto.NewStmt(".get ingestion resources"), kusto.IngestionEndpoint())
	if err != nil {
		return fmt.Errorf("problem getting ingestion resources from Kusto: %s", err)
	}

	ingest := Ingestion{}
	err = rows.Do(
		func(r *table.Row) error {
			rec := ingestResc{}
			if err := r.ToStruct(&rec); err != nil {
				return err
			}
			if err := ingest.importRec(rec); err != nil && err != errDoNotCare {
				return err
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("problem reading ingestion resources from Kusto: %s", err)
	}

	m.resources.Store(ingest)

	return nil
}

func (m *Manager) fetchRetry(ctx context.Context) {
	attempts := 0
	for {
		select {
		case <-m.done:
			return
		default:
		}

		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.fetch(ctx)
		cancel()
		if err != nil {
			attempts++
			//log.Printf("problem fetching the resources from Kusto Mgmt(attempt %d): %s", attempts, err)
			time.Sleep(10 * time.Second)
			continue
		}
		return
	}
}

// Resources returns information about the ingestion resources. This will used cached information instead
// of fetching from source.
func (m *Manager) Resources() (Ingestion, error) {
	i, ok := m.resources.Load().(Ingestion)
	if !ok {
		return Ingestion{}, fmt.Errorf("manager has not retrieved an Ingestion object yet")
	}
	return i, nil
}
