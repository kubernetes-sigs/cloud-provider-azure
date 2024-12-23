package resources

type StorageAccountStats struct {
	successCount int
	totalCount   int
}

func (s *StorageAccountStats) logResult(success bool) {
	s.totalCount++
	if success {
		s.successCount++
	}
}

func (s *StorageAccountStats) reset() {
	s.successCount = 0
	s.totalCount = 0
}

type RankedStorageAccount struct {
	buckets            []StorageAccountStats
	lastUpdateTime     int64
	currentBucketIndex int
	accountName        string
	numberOfBuckets    int
	bucketDuration     int64
	timeProvider       func() int64
}

func newRankedStorageAccount(accountName string, numberOfBuckets int, bucketDuration int64, timeProvider func() int64) *RankedStorageAccount {
	buckets := make([]StorageAccountStats, numberOfBuckets)
	return &RankedStorageAccount{
		buckets:            buckets,
		lastUpdateTime:     timeProvider(),
		currentBucketIndex: 0,
		accountName:        accountName,
		numberOfBuckets:    numberOfBuckets,
		bucketDuration:     bucketDuration,
		timeProvider:       timeProvider,
	}
}

func (r *RankedStorageAccount) adjustForTimePassed() int {
	currentTime := r.timeProvider()
	timeDelta := currentTime - r.lastUpdateTime
	windowSize := 0
	if timeDelta >= r.bucketDuration {
		r.lastUpdateTime = currentTime
		windowSize = int(timeDelta / r.bucketDuration)
		for i := 1; i < windowSize+1; i++ {
			indexToReset := (r.currentBucketIndex + i) % r.numberOfBuckets
			r.buckets[indexToReset].reset()
		}
	}
	return (r.currentBucketIndex + windowSize) % r.numberOfBuckets
}

func (r *RankedStorageAccount) logResult(success bool) {
	r.currentBucketIndex = r.adjustForTimePassed()
	r.buckets[r.currentBucketIndex].logResult(success)
}

func (r *RankedStorageAccount) getAccountName() string {
	return r.accountName
}

func (s *RankedStorageAccount) getRank() float64 {
	rank := 0.0
	totalWeight := 0
	for i := 1; i <= s.numberOfBuckets; i++ {
		bucketIndex := (s.currentBucketIndex + i) % s.numberOfBuckets
		bucket := s.buckets[bucketIndex]
		if bucket.totalCount == 0 {
			continue
		}
		successRate := float64(bucket.successCount) / float64(bucket.totalCount)
		rank += successRate * float64(i)
		totalWeight += i
	}

	if totalWeight == 0 {
		return 1.0
	}
	return rank / float64(totalWeight)
}
