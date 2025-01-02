package resources

import (
	"math/rand"
	"sync"
	"time"
)

const (
	defaultNumberOfBuckets         = 6
	defaultBucketDurationInSeconds = 10
)

var defaultTiersValue = [4]int{90, 70, 30, 0}
var defaultTimeProvider = func() int64 { return time.Now().Unix() }

type RankedStorageAccountSet struct {
	accounts        map[string]*RankedStorageAccount
	numberOfBuckets int
	bucketDuration  int64
	tiers           []int
	timeProvider    func() int64
	lock            sync.Mutex
}

func newRankedStorageAccountSet(
	numberOfBuckets int,
	bucketDuration int64,
	tiers []int,
	timeProvider func() int64,
) *RankedStorageAccountSet {
	return &RankedStorageAccountSet{
		accounts:        make(map[string]*RankedStorageAccount),
		numberOfBuckets: numberOfBuckets,
		bucketDuration:  bucketDuration,
		tiers:           tiers,
		timeProvider:    timeProvider,
		lock:            sync.Mutex{},
	}
}

func newDefaultRankedStorageAccountSet() *RankedStorageAccountSet {
	return newRankedStorageAccountSet(defaultNumberOfBuckets, defaultBucketDurationInSeconds, defaultTiersValue[:], defaultTimeProvider)
}

func (r *RankedStorageAccountSet) addAccountResult(accountName string, success bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	account, ok := r.accounts[accountName]
	if ok {
		account.logResult(success)
	}
}

func (r *RankedStorageAccountSet) registerStorageAccount(accountName string) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if _, ok := r.accounts[accountName]; !ok {
		r.accounts[accountName] = newRankedStorageAccount(accountName, r.numberOfBuckets, r.bucketDuration, r.timeProvider)
	}
}

func (r *RankedStorageAccountSet) getStorageAccount(accountName string) (*RankedStorageAccount, bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	account, ok := r.accounts[accountName]
	return account, ok
}

func (r *RankedStorageAccountSet) getRankedShuffledAccounts() []RankedStorageAccount {
	r.lock.Lock()
	defer r.lock.Unlock()

	accountsByTier := make([][]RankedStorageAccount, len(r.tiers))
	for i := range accountsByTier {
		accountsByTier[i] = []RankedStorageAccount{}
	}

	for _, account := range r.accounts {
		rankPercentage := int(account.getRank() * 100.0)
		for i := range r.tiers {
			if rankPercentage >= r.tiers[i] {
				accountsByTier[i] = append(accountsByTier[i], *account)
				break
			}
		}
	}

	var result []RankedStorageAccount

	for _, tier := range accountsByTier {
		rand.Shuffle(len(tier), func(i, j int) {
			tier[i], tier[j] = tier[j], tier[i]
		})

		//Apend to results
		result = append(result, tier...)
	}

	return result
}
