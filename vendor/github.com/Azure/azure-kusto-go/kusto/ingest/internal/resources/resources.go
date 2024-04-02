// Package resources contains objects that are used to gather information about Kusto resources that are
// used during various ingestion methods.
package resources

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-kusto-go/kusto"
	kustoErrors "github.com/Azure/azure-kusto-go/kusto/data/errors"
	"github.com/Azure/azure-kusto-go/kusto/data/table"
	"github.com/Azure/azure-kusto-go/kusto/kql"
	"github.com/cenkalti/backoff/v4"
)

const (
	defaultInitialInterval = 1 * time.Second
	defaultMultiplier      = 2
	retryCount             = 4
	fetchInterval          = 1 * time.Hour
)

// mgmter is a private interface that allows us to write hermetic tests against the kusto.Client.Mgmt() method.
type mgmter interface {
	Mgmt(ctx context.Context, db string, query kusto.Statement, options ...kusto.MgmtOption) (*kusto.RowIterator, error)
}

// URI represents a resource URI for an ingestion command.
type URI struct {
	u                   *url.URL
	account, objectName string
	sas                 url.Values
}

// Parse parses a string representing a Kutso resource URI.
func Parse(resourceUri string) (*URI, error) {
	// Example for a valid url:
	// https://fkjsalfdks.blob.core.windows.com/sdsadsadsa?sas=asdasdasd

	u, err := url.Parse(resourceUri)
	if err != nil {
		return nil, err
	}

	if u.Scheme != "https" {
		return nil, fmt.Errorf("URI scheme must be 'https', was '%s'", u.Scheme)
	}

	v := &URI{
		u:          u,
		account:    u.Hostname(),
		objectName: strings.TrimLeft(u.EscapedPath(), "/"),
		sas:        u.Query(),
	}

	if err := v.validate(); err != nil {
		return nil, err
	}

	return v, nil
}

func (u *URI) validate() error {
	if u.objectName == "" {
		return fmt.Errorf("object name was not provided")
	}
	return nil
}

// Account is the Azure storage account that will be used.
func (u *URI) Account() string {
	return u.account
}

// ObjectName returns the object name of the resource, i.e container name.
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
	client                   mgmter
	done                     chan struct{}
	resources                atomic.Value // Stores Ingestion
	lastFetchTime            atomic.Value // Stores time.Time
	kustoToken               token
	authTokenCacheExpiration time.Time
	authLock                 sync.Mutex
	fetchLock                sync.Mutex
	rankedStorageAccount     *RankedStorageAccountSet
}

// New is the constructor for Manager.
func New(client mgmter) (*Manager, error) {
	m := &Manager{client: client, done: make(chan struct{}), rankedStorageAccount: newDefaultRankedStorageAccountSet()}
	m.authLock = sync.Mutex{}
	m.fetchLock = sync.Mutex{}

	m.authTokenCacheExpiration = time.Now().UTC()
	go m.renewResources()

	return m, nil
}

// Close closes the manager. This stops any token refreshes.
func (m *Manager) Close() {
	for {
		select {
		case <-m.done:
			return
		default:
			close(m.done)
			return
		}
	}
}

func (m *Manager) renewResources() {
	tickDuration := 30 * time.Second

	tick := time.NewTicker(tickDuration)
	count := fetchInterval // Start with a fetch immediately.

	for {
		select {
		case <-tick.C:
			count += tickDuration
			if count >= fetchInterval {
				count = 0 * time.Second
				m.fetchRetry(context.Background())
			}
		case <-m.done:
			tick.Stop()
			return
		}
	}
}

// AuthContext returns a string representing the authorization context. This auth token is a temporary token
// that can be used to write a message via ingestion.  This is different than the ADAL token.
func (m *Manager) AuthContext(ctx context.Context) (string, error) {
	m.authLock.Lock()
	defer m.authLock.Unlock()
	if m.authTokenCacheExpiration.After(time.Now().UTC()) {
		return m.kustoToken.AuthContext, nil
	}

	var rows *kusto.RowIterator
	retryCtx := backoff.WithContext(initBackoff(), ctx)
	err := backoff.Retry(func() error {
		var err error
		rows, err = m.client.Mgmt(ctx, "NetDefaultDB", kql.New(".get kusto identity token"), kusto.IngestionEndpoint())
		if err == nil {
			return nil
		}
		if httpErr, ok := err.(*kustoErrors.HttpError); ok {
			// only retry in case of throttling
			if httpErr.IsThrottled() {
				return err
			}
		}
		return backoff.Permanent(err)
	}, retryCtx)

	if err != nil {
		return "", fmt.Errorf("problem getting authorization context from Kusto via Mgmt: %s", err)
	}

	count := 0
	token := token{}
	err = rows.DoOnRowOrError(
		func(r *table.Row, e *kustoErrors.Error) error {
			if e != nil {
				return e
			}
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
	m.authTokenCacheExpiration = time.Now().UTC().Add(time.Hour)
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
	//
}

var errDoNotCare = errors.New("don't care about this")

func (i *Ingestion) importRec(rec ingestResc, rankedStorageAccounts *RankedStorageAccountSet) error {
	u, err := Parse(rec.Root)
	if err != nil {
		return fmt.Errorf("the StorageRoot URI received(%s) has an error: %s", rec.Root, err)
	}

	switch rec.Type {
	case "TempStorage":
		i.Containers = append(i.Containers, u)
		rankedStorageAccounts.registerStorageAccount(u.Account())
	case "SecuredReadyForAggregationQueue":
		i.Queues = append(i.Queues, u)
		rankedStorageAccounts.registerStorageAccount(u.Account())
	case "IngestionsStatusTable":
		i.Tables = append(i.Tables, u)
	default:
		return errDoNotCare
	}
	return nil
}

// Returns a list of ranked storage account resources distributed by round robin.
func groupResourcesByStorageAccount(resources []*URI, rankedStorageAccount []RankedStorageAccount) []*URI {
	// Group the resources by storage account.
	storageAccounts := make(map[string][]*URI)
	for _, resource := range resources {
		storageAccounts[resource.Account()] = append(storageAccounts[resource.Account()], resource)
	}

	// Rank the resources by storage account.
	var rankedResources []*URI
	for _, account := range rankedStorageAccount {
		if resources, ok := storageAccounts[account.getAccountName()]; ok {
			rankedResources = append(rankedResources, resources...)
		}
	}

	//Distribute the resources by round robin.
	var distributedResources []*URI
	for i := 0; i < len(rankedResources); i++ {
		distributedResources = append(distributedResources, rankedResources[i%len(rankedResources)])
	}

	return distributedResources
}

func (i *Ingestion) getRankedStorageContainers(rankedStorageAccounts []RankedStorageAccount) []*URI {
	return groupResourcesByStorageAccount(i.Containers, rankedStorageAccounts)
}

func (i *Ingestion) getRankedStorageQueues(rankedStorageAccounts []RankedStorageAccount) []*URI {
	return groupResourcesByStorageAccount(i.Queues, rankedStorageAccounts)
}

// fetch makes a kusto.Client.Mgmt() call to retrieve the resources used for Ingestion.
func (m *Manager) fetch(ctx context.Context) error {
	m.fetchLock.Lock()
	defer m.fetchLock.Unlock()

	var rows *kusto.RowIterator
	retryCtx := backoff.WithContext(initBackoff(), ctx)
	err := backoff.Retry(func() error {
		var err error
		rows, err = m.client.Mgmt(ctx, "NetDefaultDB", kql.New(".get ingestion resources"), kusto.IngestionEndpoint())
		if err == nil {
			return nil
		}
		if httpErr, ok := err.(*kustoErrors.HttpError); ok {
			// only retry in case of throttling
			if httpErr.IsThrottled() {
				return err
			}
		}
		return backoff.Permanent(err)
	}, retryCtx)

	if err != nil {
		return fmt.Errorf("problem getting ingestion resources from Kusto: %s", err)
	}

	ingest := Ingestion{}
	err = rows.DoOnRowOrError(
		func(r *table.Row, e *kustoErrors.Error) error {
			if e != nil {
				return e
			}
			rec := ingestResc{}
			if err := r.ToStruct(&rec); err != nil {
				return err
			}
			if err := ingest.importRec(rec, m.rankedStorageAccount); err != nil && err != errDoNotCare {
				return err
			}
			return nil
		},
	)
	if err != nil {
		return fmt.Errorf("problem reading ingestion resources from Kusto: %s", err)
	}

	m.resources.Store(ingest)

	m.lastFetchTime.Store(time.Now().UTC())

	return nil
}

func (m *Manager) fetchRetry(ctx context.Context) error {
	attempts := 0
	for {

		select {
		case <-m.done:
			return nil
		default:
		}

		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err := m.fetch(ctx)
		cancel()
		if err != nil {
			attempts++
			if attempts > retryCount {
				return fmt.Errorf("failed to fetch ingestion resources: %w", err)
			}
			time.Sleep(10 * time.Second)
			continue
		}
		return nil
	}
}

func initBackoff() backoff.BackOff {
	exp := backoff.NewExponentialBackOff()
	exp.InitialInterval = defaultInitialInterval
	exp.Multiplier = defaultMultiplier
	return backoff.WithMaxRetries(exp, retryCount)
}

// Resources returns information about the ingestion resources. This will used cached information instead
// of fetching from source.
func (m *Manager) getResources() (Ingestion, error) {
	lastFetchTime, ok := m.lastFetchTime.Load().(time.Time)
	if !ok || lastFetchTime.Add(2*fetchInterval).Before(time.Now().UTC()) {
		err := m.fetchRetry(context.Background())
		if err != nil {
			return Ingestion{}, err
		}
	}

	i, ok := m.resources.Load().(Ingestion)
	if !ok {
		return Ingestion{}, fmt.Errorf("manager has not retrieved an Ingestion object yet")
	}
	return i, nil
}

// Report storage account resource usage results.
func (m *Manager) ReportStorageResourceResult(accountName string, success bool) {
	m.rankedStorageAccount.addAccountResult(accountName, success)
}

// Get ranked containers
func (m *Manager) GetRankedStorageContainers() ([]*URI, error) {
	ingestionResources, err := m.getResources()
	if err != nil {
		return nil, err
	}
	return ingestionResources.getRankedStorageContainers(m.rankedStorageAccount.getRankedShuffledAccounts()), nil
}

// get ranked queues
func (m *Manager) GetRankedStorageQueues() ([]*URI, error) {
	ingestionResources, err := m.getResources()
	if err != nil {
		return nil, err
	}
	return ingestionResources.getRankedStorageQueues(m.rankedStorageAccount.getRankedShuffledAccounts()), nil
}

func (m *Manager) GetTables() ([]*URI, error) {
	ingestionResources, err := m.getResources()
	if err != nil {
		return nil, err
	}
	return ingestionResources.Tables, nil
}
