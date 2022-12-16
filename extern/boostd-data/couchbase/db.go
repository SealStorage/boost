package couchbase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/couchbase/gocb/v2"
	"github.com/filecoin-project/boost/tracing"
	"github.com/filecoin-project/boostd-data/model"
	"github.com/ipfs/go-cid"
	"github.com/multiformats/go-multihash"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/errgroup"
)

const pieceDirBucketPrefix = "piece-dir."
const pieceCidToMetadataBucket = pieceDirBucketPrefix + "piece-metadata"
const multihashToPiecesBucket = pieceDirBucketPrefix + "mh-to-pieces"
const pieceOffsetsBucket = pieceDirBucketPrefix + "piece-offsets"
const metaBucket = pieceDirBucketPrefix + "metadata"

// The maximum length for a couchbase key is 250 bytes, but we don't need a
// key that long, 128 bytes is more than enough
const maxCouchKeyLen = 128

// maxCasRetries is the number of times to retry an update operation when
// there is a cas mismatch
const maxCasRetries = 10

type DB struct {
	settings     DBSettings
	cluster      *gocb.Cluster
	pcidToMeta   *gocb.Collection
	mhToPieces   *gocb.Collection
	pieceOffsets *gocb.Collection
}

type DBSettingsAuth struct {
	Username string
	Password string
}

type DBSettingsBucket struct {
	RAMQuotaMB uint64
}

type DBSettings struct {
	ConnectString           string
	Auth                    DBSettingsAuth
	PieceMetadataBucket     DBSettingsBucket
	MultihashToPiecesBucket DBSettingsBucket
	PieceOffsetsBucket      DBSettingsBucket
	// Period between checking each piece in the database for problems (eg missing index)
	PieceCheckPeriod time.Duration
	TestMode         bool
}

const connectTimeout = 5 * time.Second
const kvTimeout = 30 * time.Second

func newDB(ctx context.Context, settings DBSettings) (*DB, error) {
	cluster, err := gocb.Connect(settings.ConnectString, gocb.ClusterOptions{
		TimeoutsConfig: gocb.TimeoutsConfig{
			ConnectTimeout: connectTimeout,
			KVTimeout:      kvTimeout,
		},
		Authenticator: gocb.PasswordAuthenticator{
			Username: settings.Auth.Username,
			Password: settings.Auth.Password,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("connecting to couchbase cluster %s: %w", settings.ConnectString, err)
	}

	pingStart := time.Now()
	err = pingCluster(ctx, cluster, settings.ConnectString)
	if err != nil {
		return nil, err
	}

	log.Infow("Connected to couchbase cluster",
		"connect-string", settings.ConnectString,
		"max-service-latency", time.Since(pingStart).String())

	// Set up the buckets
	db := &DB{settings: settings, cluster: cluster}
	pcidToMeta, err := CreateBucket(ctx, cluster, pieceCidToMetadataBucket, settings.PieceMetadataBucket.RAMQuotaMB)
	if err != nil {
		return nil, fmt.Errorf("Creating bucket %s for couchbase server %s: %w", pieceCidToMetadataBucket, settings.ConnectString, err)
	}
	db.pcidToMeta = pcidToMeta.DefaultCollection()

	mhToPieces, err := CreateBucket(ctx, cluster, multihashToPiecesBucket, settings.MultihashToPiecesBucket.RAMQuotaMB)
	if err != nil {
		return nil, fmt.Errorf("Creating bucket %s for couchbase server %s: %w", multihashToPiecesBucket, settings.ConnectString, err)
	}
	db.mhToPieces = mhToPieces.DefaultCollection()

	pieceOffsets, err := CreateBucket(ctx, cluster, pieceOffsetsBucket, settings.PieceOffsetsBucket.RAMQuotaMB)
	if err != nil {
		return nil, fmt.Errorf("Creating bucket %s for couchbase server %s: %w", pieceOffsetsBucket, settings.ConnectString, err)
	}
	db.pieceOffsets = pieceOffsets.DefaultCollection()

	meta, err := CreateBucket(ctx, cluster, metaBucket, 156)
	if err != nil {
		return nil, fmt.Errorf("Creating bucket %s for couchbase server %s: %w", metaBucket, settings.ConnectString, err)
	}

	err = createCollection(ctx, cluster, meta, "piece-tracker")
	if err != nil {
		return nil, err
	}
	err = createCollection(ctx, cluster, meta, "piece-flagged")
	if err != nil {
		return nil, err
	}

	return db, nil
}

func createCollection(ctx context.Context, cluster *gocb.Cluster, bucket *gocb.Bucket, collName string) error {
	err := bucket.Collections().CreateCollection(gocb.CollectionSpec{
		Name:      collName,
		ScopeName: bucket.DefaultScope().Name(),
	}, &gocb.CreateCollectionOptions{Context: ctx})
	if err != nil {
		if !errors.Is(err, gocb.ErrCollectionExists) {
			return fmt.Errorf("Creating %s collection: %w", collName, err)
		}
	}

	time.Sleep(time.Second)

	err = cluster.QueryIndexes().CreatePrimaryIndex(bucket.Name(), &gocb.CreatePrimaryQueryIndexOptions{
		Context:        ctx,
		ScopeName:      bucket.DefaultScope().Name(),
		CollectionName: collName,
		IgnoreIfExists: true,
	})
	if err != nil {
		return fmt.Errorf("creating primary index on %s.%s: %w", bucket.Name(), collName, err)
	}

	return nil
}

func pingCluster(ctx context.Context, cluster *gocb.Cluster, connectString string) error {
	res, err := cluster.Ping(&gocb.PingOptions{
		Timeout: connectTimeout,
		Context: ctx,
	})
	if err == nil {
		for svc, png := range res.Services {
			if len(png) > 0 && png[0].State != gocb.PingStateOk {
				err = fmt.Errorf("connecting to %s service", ServiceName(svc))
				break
			}
		}
	}

	if err != nil {
		msg := fmt.Sprintf("Connecting to couchbase server %s", connectString)
		return fmt.Errorf(msg+": %w\nCheck the couchbase server is running and the username / password are correct", err)
	}

	return nil
}

func CreateBucket(ctx context.Context, cluster *gocb.Cluster, bucketName string, ramMb uint64) (*gocb.Bucket, error) {
	_, err := cluster.Buckets().GetBucket(bucketName, &gocb.GetBucketOptions{Context: ctx, Timeout: connectTimeout})
	if err != nil {
		if !errors.Is(err, gocb.ErrBucketNotFound) {
			msg := fmt.Sprintf("getting bucket %s", bucketName)
			return nil, fmt.Errorf(msg+": %w\nCheck the couchbase server is running and the username / password are correct", err)
		}

		err = cluster.Buckets().CreateBucket(gocb.CreateBucketSettings{
			BucketSettings: gocb.BucketSettings{
				Name:       bucketName,
				RAMQuotaMB: ramMb,
				BucketType: gocb.CouchbaseBucketType,
				// The default eviction policy requires couchbase to keep all
				// keys (and metadata) in memory. So use an eviction policy
				// that allows keys to be stored on disk (but not in memory).
				EvictionPolicy: gocb.EvictionPolicyTypeFull,
			},
		}, &gocb.CreateBucketOptions{Context: ctx})
		if err != nil {
			return nil, fmt.Errorf("creating bucket %s: %w", bucketName, err)
		}

		// TODO: For some reason WaitUntilReady times out if we don't put
		// this sleep here
		time.Sleep(time.Second)
	}

	err = cluster.QueryIndexes().CreatePrimaryIndex(bucketName, &gocb.CreatePrimaryQueryIndexOptions{
		IgnoreIfExists: true,
		Context:        ctx,
	})
	if err != nil {
		return nil, fmt.Errorf("creating primary index on %s: %w", bucketName, err)
	}

	bucket := cluster.Bucket(bucketName)
	err = bucket.WaitUntilReady(5*time.Second, nil)
	if err != nil {
		return nil, fmt.Errorf("waiting for couchbase bucket to be ready: %w", err)
	}

	return bucket, nil
}

// GetPieceCidsByMultihash
func (db *DB) GetPieceCidsByMultihash(ctx context.Context, mh multihash.Multihash) ([]cid.Cid, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.get_piece_cids_by_multihash")
	defer span.End()

	k := toCouchKey(mh.String())
	var getResult *gocb.GetResult
	getResult, err := db.mhToPieces.Get(k, &gocb.GetOptions{Context: ctx})
	if err != nil {
		return nil, fmt.Errorf("getting piece cids by multihash %s: %w", mh, err)
	}

	var cidStrs []string
	err = getResult.Content(&cidStrs)
	if err != nil {
		return nil, fmt.Errorf("getting piece cids content by multihash %s: %w", mh, err)
	}

	pcids := make([]cid.Cid, 0, len(cidStrs))
	for _, c := range cidStrs {
		pcid, err := cid.Decode(c)
		if err != nil {
			return nil, fmt.Errorf("getting piece cids by multihash %s: parsing piece cid %s: %w", mh, pcid, err)
		}
		pcids = append(pcids, pcid)
	}

	return pcids, nil
}

const throttleSize = 32

// SetMultihashesToPieceCid
func (db *DB) SetMultihashesToPieceCid(ctx context.Context, mhs []multihash.Multihash, pieceCid cid.Cid) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.set_multihashes_to_piece_cid")
	defer span.End()

	throttle := make(chan struct{}, throttleSize)
	var eg errgroup.Group
	for _, mh := range mhs {
		mh := mh

		throttle <- struct{}{}
		eg.Go(func() error {
			defer func() { <-throttle }()

			return db.withCasRetry("multihash -> pieces", func() error {
				cbKey := toCouchKey(mh.String())

				// Insert a tuple into the bucket: multihash -> [piece cid]
				_, err := db.mhToPieces.Insert(cbKey, []string{pieceCid.String()}, &gocb.InsertOptions{Context: ctx})
				if err == nil {
					return nil
				}

				// If the value already exists, it's not an error, we'll just
				// add the piece cid to the existing set of piece cids
				isDocExists := errors.Is(err, gocb.ErrDocumentExists)
				if !isDocExists {
					// If there was some other error, return it
					return fmt.Errorf("adding multihash %s to piece %s: insert doc: %w", mh, pieceCid, err)
				}

				// Add the piece cid to the set of piece cids
				mops := []gocb.MutateInSpec{
					gocb.ArrayAddUniqueSpec("", pieceCid.String(), &gocb.ArrayAddUniqueSpecOptions{}),
				}
				_, err = db.mhToPieces.MutateIn(cbKey, mops, &gocb.MutateInOptions{Context: ctx})
				if err != nil {
					if errors.Is(err, gocb.ErrPathExists) {
						// If the set of piece cids already contains the piece,
						// it's not an error, just return nil
						return nil
					}
					return fmt.Errorf("adding multihash %s to piece %s: mutate doc: %w", mh, pieceCid, err)
				}

				return nil
			})
		})
	}

	return eg.Wait()
}

func (db *DB) SetCarSize(ctx context.Context, pieceCid cid.Cid, size uint64) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.set_car_size")
	defer span.End()

	return db.mutatePieceMetadata(ctx, pieceCid, "set-car-size", func(metadata CouchbaseMetadata) *CouchbaseMetadata {
		// Set the car size on each deal (should be the same for all deals)
		var deals []model.DealInfo
		for _, dl := range metadata.Deals {
			dl.CarLength = size

			deals = append(deals, dl)
		}
		metadata.Deals = deals
		return &metadata
	})
}

func (db *DB) MarkIndexErrored(ctx context.Context, pieceCid cid.Cid, idxErr error) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.mark_piece_index_errored")
	defer span.End()

	return db.mutatePieceMetadata(ctx, pieceCid, "mark-index-errored", func(metadata CouchbaseMetadata) *CouchbaseMetadata {
		// If the error was already set, don't overwrite it
		if metadata.Error != "" {
			// If the error state has already been set, don't over-write the existing error
			return nil
		}

		// Set the error state
		metadata.Error = idxErr.Error()
		metadata.ErrorType = fmt.Sprintf("%T", idxErr)

		return &metadata
	})
}

func (db *DB) MarkIndexingComplete(ctx context.Context, pieceCid cid.Cid, blockCount int) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.mark_indexing_complete")
	defer span.End()

	return db.mutatePieceMetadata(ctx, pieceCid, "mark-indexing-complete", func(metadata CouchbaseMetadata) *CouchbaseMetadata {
		// Mark indexing as complete
		metadata.IndexedAt = time.Now()
		metadata.BlockCount = blockCount
		if metadata.Deals == nil {
			metadata.Deals = []model.DealInfo{}
		}
		return &metadata
	})
}

func (db *DB) AddDealForPiece(ctx context.Context, pieceCid cid.Cid, dealInfo model.DealInfo) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.add_deal_for_piece")
	defer span.End()

	return db.mutatePieceMetadata(ctx, pieceCid, "add-deal-for-piece", func(md CouchbaseMetadata) *CouchbaseMetadata {
		// Check if the deal has already been added
		for _, dl := range md.Deals {
			if dl == dealInfo {
				return nil
			}
		}

		// Add the deal to the list.
		// Note: we can't use ArrayAddUniqueSpec here because it only works
		// with primitives, not objects.
		md.Deals = append(md.Deals, dealInfo)
		return &md
	})
}

type mutateMetadata func(CouchbaseMetadata) *CouchbaseMetadata

func (db *DB) mutatePieceMetadata(ctx context.Context, pieceCid cid.Cid, opName string, mutate mutateMetadata) error {
	return db.withCasRetry(opName, func() error {
		// Get the piece metadata from the db
		var md CouchbaseMetadata
		var pieceMetaExists bool
		cbKey := toCouchKey(pieceCid.String())
		getResult, err := db.pcidToMeta.Get(cbKey, &gocb.GetOptions{Context: ctx})
		if err == nil {
			pieceMetaExists = true
			err = getResult.Content(&md)
			if err != nil {
				return fmt.Errorf("getting piece cid to metadata content for piece %s: %w", pieceCid, err)
			}
		} else if !isNotFoundErr(err) {
			return fmt.Errorf("getting piece cid metadata for piece %s: %w", pieceCid, err)
		}

		// Apply the mutation to the metadata
		newMetadata := mutate(md)
		if newMetadata == nil {
			// If there was no mutation applied, just return immediately
			return nil
		}

		// Write the piece metadata back to the db
		if pieceMetaExists {
			_, err = db.pcidToMeta.Replace(cbKey, newMetadata, &gocb.ReplaceOptions{
				Context: ctx,
				Cas:     getResult.Cas(),
			})
		} else {
			_, err = db.pcidToMeta.Insert(cbKey, newMetadata, &gocb.InsertOptions{Context: ctx})
		}
		if err != nil {
			return fmt.Errorf("setting piece %s metadata: %w", pieceCid, err)
		}

		return nil
	})
}

// GetPieceCidToMetadata
func (db *DB) GetPieceCidToMetadata(ctx context.Context, pieceCid cid.Cid) (CouchbaseMetadata, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.get_piece_cid_to_metadata")
	defer span.End()

	var getResult *gocb.GetResult
	k := toCouchKey(pieceCid.String())
	getResult, err := db.pcidToMeta.Get(k, &gocb.GetOptions{Context: ctx})
	if err != nil {
		return CouchbaseMetadata{}, fmt.Errorf("getting piece cid to metadata for piece %s: %w", pieceCid, err)
	}

	var metadata CouchbaseMetadata
	err = getResult.Content(&metadata)
	if err != nil {
		return CouchbaseMetadata{}, fmt.Errorf("getting piece cid to metadata content for piece %s: %w", pieceCid, err)
	}

	return metadata, nil
}

// GetOffsetSize gets the offset and size of the multihash in the given piece.
// Note that recordCount is needed in order to determine which shard the multihash is in.
func (db *DB) GetOffsetSize(ctx context.Context, pieceCid cid.Cid, hash multihash.Multihash, recordCount int) (*model.OffsetSize, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.get_offset_size")
	defer span.End()

	// Get the prefix for the shard that the multihash is in
	shardPrefixBitCount, _ := getShardPrefixBitCount(recordCount)
	mask := get2ByteMask(shardPrefixBitCount)
	shardPrefix := hashToShardPrefix(hash, mask)

	// Get the map of multihash -> offset/size.
	// Note: This doesn't actually fetch the map, it just gets a reference to it.
	cbKey := toCouchKey(pieceCid.String() + shardPrefix)
	cbMap := db.pieceOffsets.Map(cbKey)

	// Get the offset/size from the map.
	// Note: This doesn't actually fetch the whole map, it tells couchbase to
	// find the key in the map on the server side, and return the value.
	var val string
	err := cbMap.At(hash.String(), &val)
	if err != nil {
		return nil, fmt.Errorf("getting offset/size for piece %s multihash %s: %w", pieceCid, hash, err)
	}

	var ofsz model.OffsetSize
	err = ofsz.UnmarshallBase64(val)
	if err != nil {
		return nil, fmt.Errorf("parsing piece %s offset / size value '%s': %w", pieceCid, val, err)
	}
	return &ofsz, nil
}

// AllRecords gets all the mulithash -> offset/size mappings in a given piece.
// Note that recordCount is needed in order to determine the shard structure.
func (db *DB) AllRecords(ctx context.Context, pieceCid cid.Cid, recordCount int) ([]model.Record, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.all_records")
	defer span.End()

	// Get the number of shards
	_, totalShards := getShardPrefixBitCount(recordCount)

	recs := make([]model.Record, 0, recordCount)
	recsShard := make([][]model.Record, totalShards)

	var eg errgroup.Group

	span.SetAttributes(attribute.Int("shards", totalShards))
	span.SetAttributes(attribute.Int("recs", recordCount))
	for i := 0; i < totalShards; i++ {
		i := i
		recsShard[i] = make([]model.Record, 0, recordCount)
		eg.Go(func() error {
			// Get the map of multihash -> offset/size for the shard
			shardPrefix, err := getShardPrefix(i)
			if err != nil {
				return err
			}
			cbKey := toCouchKey(pieceCid.String() + shardPrefix)
			cbMap := db.pieceOffsets.Map(cbKey)

			_, spanIter := tracing.Tracer.Start(ctx, "db.iter")
			recMap, err := cbMap.Iterator()
			spanIter.End()
			if err != nil {
				if isNotFoundErr(err) {
					// If there are no records in a particular shard just skip the shard
					return nil
				}
				return fmt.Errorf("getting all records for piece %s: %w", pieceCid, err)
			}

			span.SetAttributes(attribute.Int(fmt.Sprintf("map_%d", i), len(recMap)))

			_, spanMap := tracing.Tracer.Start(ctx, "db.recMap")

			// Get each value in the map
			for mhStr, offsetSizeIfce := range recMap {
				mh, err := multihash.FromHexString(mhStr)
				if err != nil {
					return fmt.Errorf("parsing piece cid %s multihash value '%s': %w", pieceCid, mhStr, err)
				}

				val, ok := offsetSizeIfce.(string)
				if !ok {
					return fmt.Errorf("unexpected type for piece cid %s offset/size value: %T", pieceCid, offsetSizeIfce)
				}

				var ofsz model.OffsetSize
				err = ofsz.UnmarshallBase64(val)
				if err != nil {
					return fmt.Errorf("parsing piece %s offset / size value '%s': %w", pieceCid, val, err)
				}

				recsShard[i] = append(recsShard[i], model.Record{Cid: cid.NewCidV1(cid.Raw, mh), OffsetSize: ofsz})
			}

			spanMap.End()

			return nil
		})

	}

	err := eg.Wait()
	if err != nil {
		return nil, err
	}

	for i := 0; i < totalShards; i++ {
		recs = append(recs, recsShard[i]...)
	}

	return recs, nil
}

// Couchbase has an upper limit on the size of a value: 20mb
// A JSON-encoded map with 128k keys results in a value of about 8mb in size
// so this is well under the 20mb limit.
var maxRecsPerShard = 128 * 1024

// Limit the number of shards to 2048. This means there is an upper limit of
// ~270 million blocks per piece, which should be more than enough:
// 64 Gib piece / (2048 * 128 * 1024) = 238 bytes per block
const maxShardsPerPiece = 2048

var maxRecsPerPiece = maxShardsPerPiece * maxRecsPerShard

func getShardPrefixBitCount(recordCount int) (int, int) {
	// The number of shards required to store all the keys
	requiredShards := (recordCount / maxRecsPerShard) + 1
	// The number of shards that will be created must be a power of 2,
	// so get the first power of two that's >= requiredShards
	shardPrefixBits := 0
	totalShards := 1
	for totalShards < requiredShards {
		shardPrefixBits += 1
		totalShards *= 2
	}

	return shardPrefixBits, totalShards
}

func getShardPrefix(shardIndex int) (string, error) {
	if shardIndex >= 1<<16 {
		return "", fmt.Errorf("shard index of size %d does not fit into 2 byte prefix", shardIndex)
	}

	shardPrefix := []byte{0, 0}
	shardPrefix[1] = byte(shardIndex)
	shardPrefix[0] = byte(shardIndex >> 8)
	return string(shardPrefix), nil
}

// AddIndexRecords
func (db *DB) AddIndexRecords(ctx context.Context, pieceCid cid.Cid, recs []model.Record) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.add_index_records")
	span.SetAttributes(attribute.Int("recs", len(recs)))
	defer span.End()

	if len(recs) > maxRecsPerPiece {
		return fmt.Errorf("index for piece %s too large: %d records (size limit is %d)", pieceCid, len(recs), maxRecsPerPiece)
	}

	// Get the number of bits in the shard prefix, and the total number of shards
	shardPrefixBitCount, totalShards := getShardPrefixBitCount(len(recs))

	// Initialize the multihash -> offset/size map for each shard
	type mhToOffsetSizeMap map[string]string
	shardMaps := make(map[string]mhToOffsetSizeMap, totalShards)
	for i := 0; i < totalShards; i++ {
		shardPrefix, err := getShardPrefix(i)
		if err != nil {
			return err
		}
		shardMaps[shardPrefix] = make(map[string]string)
	}

	// Create a mask of the required number of bits
	// eg 3 bit mask = 0000 0000 0000 0111
	mask := get2ByteMask(shardPrefixBitCount)

	// For each record
	for _, rec := range recs {
		// Apply the bit mask to the last 2 bytes of the multihash to get the
		// shard prefix
		hash := rec.Cid.Hash()
		shardPrefix := hashToShardPrefix(hash, mask)

		// Add the record to the shard's map
		shardMaps[shardPrefix][hash.String()] = rec.MarshallBase64()
	}

	// Add each shard's map to couchbase
	for shardPrefix, shardMap := range shardMaps {
		if len(shardMap) == 0 {
			continue
		}

		cbKey := toCouchKey(pieceCid.String() + shardPrefix)
		_, err := db.pieceOffsets.Upsert(cbKey, shardMap, &gocb.UpsertOptions{Context: ctx})
		if err != nil {
			return fmt.Errorf("adding offset / sizes for piece %s: %w", pieceCid, err)
		}
	}

	return nil
}

func (db *DB) ListPieces(ctx context.Context) ([]cid.Cid, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.list_pieces")
	defer span.End()

	res, err := db.query(ctx, "SELECT META().id FROM `"+pieceCidToMetadataBucket+"`")
	if err != nil {
		return nil, fmt.Errorf("getting keys from %s: %w", pieceCidToMetadataBucket, err)
	}

	return db.listPieces(res)
}

const piecesToTrackerBatchSize = 1024
const trackerCheckBatchSize = 1024

func (db *DB) NextPiecesToCheck(ctx context.Context) ([]cid.Cid, error) {
	keepInserting := true
	for keepInserting {
		// Add any new pieces into the piece status tracking table
		qry := "INSERT INTO `" + metaBucket + "`._default.`piece-tracker` (KEY k, VALUE v) "
		qry += "SELECT "
		qry += "  META(pieceMeta).id AS k, "
		qry += "  {'CreatedAt': NOW_LOCAL(), 'UpdatedAt': null} AS v "
		qry += "FROM `" + pieceCidToMetadataBucket + "` AS pieceMeta "
		qry += "WHERE META(pieceMeta).id NOT IN ("
		qry += "  SELECT RAW META(pieceTracker).id FROM `" + metaBucket + "`._default.`piece-tracker` AS pieceTracker"
		qry += ") "
		qry += fmt.Sprintf("LIMIT %d", piecesToTrackerBatchSize)

		fmt.Println(qry)
		res, err := db.mutate(ctx, qry)
		if err != nil {
			return nil, fmt.Errorf("executing insert into piece-tracker: %w", err)
		}

		// If there were enough remaining rows to fill an entire batch,
		// keep inserting a new batch of rows
		queryMeta, err := res.MetaData()
		if err != nil {
			return nil, fmt.Errorf("getting query metadata: %w", err)
		}
		keepInserting = queryMeta.Metrics.MutationCount == piecesToTrackerBatchSize
	}

	// Get all pieces from the piece tracker table that have not been updated
	// since the last piece check period.
	// Simultaneously set the UpdatedAt field so that these pieces are marked
	// as checked (and will not be returned until the next piece check period
	// elapses again).
	qry := "UPDATE `" + metaBucket + "`._default.`piece-tracker` as outerPT "
	qry += "SET UpdatedAt = NOW_LOCAL() "
	qry += "WHERE META(outerPT).id IN ("
	qry += "  SELECT RAW META(innerPT).id FROM `" + metaBucket + "`._default.`piece-tracker` AS innerPT "
	qry += "  WHERE "
	qry += "    innerPT.UpdatedAt IS NULL OR"
	qry += "    innerPT.UpdatedAt <= DATE_ADD_STR(NOW_LOCAL(), ?, 'millisecond') "
	qry += fmt.Sprintf("LIMIT %d", trackerCheckBatchSize)
	qry += ") "
	qry += "RETURNING META().id"

	fmt.Println(qry, -db.settings.PieceCheckPeriod.Milliseconds())
	res, err := db.query(ctx, qry, -db.settings.PieceCheckPeriod.Milliseconds())
	if err != nil {
		return nil, fmt.Errorf("adding piece meta info to piece tracker table: %w", err)
	}

	pcids, err := db.listPieces(res)
	if err != nil {
		queryMeta, err := res.MetaData()
		if err != nil {
			return nil, err
		}
		fmt.Printf("updated/got %d rows in piece-tracker\n", queryMeta.Metrics.MutationCount)
	}

	return pcids, err
}

func (db *DB) FlagPiece(ctx context.Context, pieceCid cid.Cid) error {
	qry := "INSERT INTO `" + metaBucket + "`._default.`piece-flagged` (KEY, VALUE) "
	qry += "VALUES (?, { 'CreatedAt': NOW_LOCAL(), 'UpdatedAt': NOW_LOCAL() })"
	_, err := db.mutate(ctx, qry, toCouchKey(pieceCid.String()))
	if err != nil {
		if !errors.Is(err, gocb.ErrDocumentExists) {
			return fmt.Errorf("flagging piece %s: inserting row: %w", pieceCid, err)
		}

		qry := "UPDATE `" + metaBucket + "`._default.`piece-flagged` "
		qry += "SET UpdatedAt = NOW_LOCAL() "
		qry += "WHERE META(id) = ?"
		_, err = db.mutate(ctx, qry, toCouchKey(pieceCid.String()))
		if err != nil {
			return fmt.Errorf("flagging piece %s: updating row: %w", pieceCid, err)
		}
	}
	return nil
}

func (db *DB) UnflagPiece(ctx context.Context, pieceCid cid.Cid) error {
	qry := "DELETE FROM `" + metaBucket + "`._default.`piece-flagged` WHERE META().id = ?"
	_, err := db.mutate(ctx, qry, toCouchKey(pieceCid.String()))
	if err != nil {
		return fmt.Errorf("unflagging piece %s: %w", pieceCid, err)
	}
	return nil
}

func (db *DB) FlaggedPiecesList(ctx context.Context, cursor *time.Time, offset int, limit int) ([]model.FlaggedPiece, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.list_flagged_pieces")
	defer span.End()

	args := []interface{}{}
	tableName := "`" + metaBucket + "`._default.`piece-flagged`"
	qry := "SELECT META(pieceFlagged).id, CreatedAt FROM " + tableName + " AS pieceFlagged "
	if cursor != nil {
		qry += "WHERE CreatedAt < ? "
		args = append(args, cursor)
	}
	qry += "ORDER BY CreatedAt desc "

	qry += "LIMIT ? OFFSET ?"
	args = append(args, limit, offset)

	res, err := db.query(ctx, qry, args...)
	if err != nil {
		return nil, fmt.Errorf("getting keys from %s: %w", tableName, err)
	}

	var pieces []model.FlaggedPiece
	var rowData map[string]string
	for res.Next() {
		err := res.Row(&rowData)
		if err != nil {
			return nil, fmt.Errorf("reading row from %s: %w", pieceCidToMetadataBucket, err)
		}

		couchKey, ok := rowData["id"]
		if !ok {
			return nil, fmt.Errorf("unexpected row data %s reading row from %s: missing id", rowData, pieceCidToMetadataBucket)
		}

		c, err := cid.Parse(fromCouchKey(couchKey))
		if err != nil {
			return nil, fmt.Errorf("parsing piece cid from couchbase key '%s': %w", couchKey, err)
		}

		createdAtStr, ok := rowData["CreatedAt"]
		if !ok {
			return nil, fmt.Errorf("unexpected row data %s reading row from %s: missing CreatedAt", rowData, pieceCidToMetadataBucket)
		}
		createdAt, err := time.Parse(time.RFC3339, createdAtStr)
		if err != nil {
			return nil, fmt.Errorf("parsing flagged piece CreatedAt from '%s': %w", couchKey, err)
		}

		pieces = append(pieces, model.FlaggedPiece{
			CreatedAt: createdAt,
			PieceCid:  c,
		})
	}

	err = res.Err()
	if err != nil {
		return nil, fmt.Errorf("reading stream from %s: %w", pieceCidToMetadataBucket, err)
	}

	return pieces, nil
}

func (db *DB) FlaggedPiecesCount(ctx context.Context) (int, error) {
	ctx, span := tracing.Tracer.Start(ctx, "db.count_flagged_pieces")
	defer span.End()

	tableName := "`" + metaBucket + "`._default.`piece-flagged`"
	qry := "SELECT COUNT(*) as cnt FROM " + tableName

	res, err := db.query(ctx, qry)
	if err != nil {
		return 0, fmt.Errorf("getting count of flagged pieces: %w", err)
	}

	var m map[string]int
	err = res.One(&m)
	if err != nil {
		return 0, fmt.Errorf("getting count of flagged pieces from result: %w", err)
	}
	count, ok := m["cnt"]
	if !ok {
		return 0, fmt.Errorf("missing expected result column in count query")
	}

	return count, err
}

func (db *DB) listPieces(res *gocb.QueryResult) ([]cid.Cid, error) {
	var pieceCids []cid.Cid
	var rowData map[string]string
	for res.Next() {
		err := res.Row(&rowData)
		if err != nil {
			return nil, fmt.Errorf("reading row from %s: %w", pieceCidToMetadataBucket, err)
		}

		couchKey, ok := rowData["id"]
		if !ok {
			return nil, fmt.Errorf("unexpected row data %s reading row from %s", rowData, pieceCidToMetadataBucket)
		}

		c, err := cid.Parse(fromCouchKey(couchKey))
		if err != nil {
			return nil, fmt.Errorf("parsing piece cid from couchbase key '%s': %w", couchKey, err)
		}

		pieceCids = append(pieceCids, c)
	}

	err := res.Err()
	if err != nil {
		return nil, fmt.Errorf("reading stream from %s: %w", pieceCidToMetadataBucket, err)
	}

	return pieceCids, nil
}

func (db *DB) query(ctx context.Context, qry string, args ...interface{}) (*gocb.QueryResult, error) {
	opts := &gocb.QueryOptions{
		Context:              ctx,
		PositionalParameters: args,
	}
	if db.settings.TestMode {
		// In test mode, require immediate consistency for reads:
		// wait for all documents to complete updating before performing
		// the query
		opts.ScanConsistency = gocb.QueryScanConsistencyRequestPlus
	}
	return db.cluster.Query(qry, opts)
}

func (db *DB) mutate(ctx context.Context, qry string, args ...interface{}) (*gocb.QueryResult, error) {
	// Execute the query
	opts := &gocb.QueryOptions{
		Context:              ctx,
		PositionalParameters: args,
	}
	res, err := db.cluster.Query(qry, opts)
	if err != nil {
		return nil, fmt.Errorf("executing mutate query: %w", err)
	}

	// We have to drain the results in order to close the stream
	for res.Next() {
	}
	err = res.Err()
	if err != nil {
		return nil, fmt.Errorf("draining query stream: %w", err)
	}

	return res, nil
}

// Attempt to perform an update operation. If the operation fails due to a
// cas mismatch, or inserting a document at a key that already exists, retry
// several times before giving up.
// Note: cas mismatch is caused when
// - there is a get + update
// - another process applied the update before this process
func (db *DB) withCasRetry(opName string, f func() error) error {
	var err error
	for i := 0; i < maxCasRetries; i++ {
		err = f()
		if err == nil {
			return nil
		}
		if !errors.Is(err, gocb.ErrCasMismatch) && !errors.Is(err, gocb.ErrDocumentExists) {
			return err
		}
	}

	if err != nil {
		log.Warnw("exceeded max compare and swap retries (%d) for "+opName+": %w", maxCasRetries, err)
	}

	return err
}

func toCouchKey(key string) string {
	k := "u:" + key
	if len(k) > maxCouchKeyLen {
		// There is usually important stuff at the beginning and end of a key,
		// so cut out the characters in the middle
		k = k[:maxCouchKeyLen/2] + k[len(k)-maxCouchKeyLen/2:]
	}
	return k
}

func fromCouchKey(couchKey string) string {
	if strings.HasPrefix(couchKey, "u:") {
		return couchKey[2:]
	}
	return couchKey
}

func isNotFoundErr(err error) bool {
	return errors.Is(err, gocb.ErrDocumentNotFound)
}

// Create a 2 byte mask of the required number of bits
// eg 3 bit mask = 0000 0000 0000 0111
func get2ByteMask(bits int) [2]byte {
	buf := [2]byte{0, 0}
	buf[1] = (1 << bits) - 1
	if bits >= 8 {
		buf[0] = (1 << (bits - 8)) - 1
	}
	return buf
}

// Apply a mask to the last two bytes of the hash to use as the shard prefix
func hashToShardPrefix(hash multihash.Multihash, mask [2]byte) string {
	return string([]byte{
		hash[len(hash)-2] & mask[0],
		hash[len(hash)-1] & mask[1],
	})
}

func ServiceName(svc gocb.ServiceType) string {
	switch svc {
	case gocb.ServiceTypeManagement:
		return "mgmt"
	case gocb.ServiceTypeKeyValue:
		return "kv"
	case gocb.ServiceTypeViews:
		return "views"
	case gocb.ServiceTypeQuery:
		return "query"
	case gocb.ServiceTypeSearch:
		return "search"
	case gocb.ServiceTypeAnalytics:
		return "analytics"
	}
	return "unknown"
}

func EndpointStateName(state gocb.EndpointState) string {
	switch state {
	case gocb.EndpointStateDisconnected:
		return "disconnected"
	case gocb.EndpointStateConnecting:
		return "connecting"
	case gocb.EndpointStateConnected:
		return "connected"
	case gocb.EndpointStateDisconnecting:
		return "disconnecting"
	}
	return ""
}

// RemoveMetadata
func (db *DB) RemovePieceMetadata(ctx context.Context, pieceCid cid.Cid) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.remove_piece_metadata")
	defer span.End()

	return db.withCasRetry("remove-metadata-for-piece", func() error {
		var getResult *gocb.GetResult
		k := toCouchKey(pieceCid.String())
		getResult, err := db.pcidToMeta.Get(k, &gocb.GetOptions{Context: ctx})
		if err != nil {
			if isNotFoundErr(err) {
				return nil
			}
			return fmt.Errorf("getting piece cid to metadata for piece %s: %w", pieceCid, err)
		}

		var metadata CouchbaseMetadata
		err = getResult.Content(&metadata)
		if err != nil {
			return fmt.Errorf("getting piece cid to metadata content for piece %s: %w", pieceCid, err)
		}

		// Remove all multihashes first, as without Metadata, they cannot be removed.
		// This order is important as metadata.BlockCount is required in case RemoveIndexes fails
		// and needs to be run manually
		if err = db.RemoveIndexes(ctx, pieceCid, metadata.BlockCount); err != nil {
			return fmt.Errorf("failed removing index for piece %s: %w", pieceCid, err)
		}

		_, err = db.pcidToMeta.Remove(k, &gocb.RemoveOptions{
			Context: ctx,
			Cas:     getResult.Cas(),
		})
		if err != nil {
			if isNotFoundErr(err) {
				return nil
			}
			return fmt.Errorf("removing piece %s metadata: %w", pieceCid, err)
		}
		return nil
	})
}

// RemoveIndexes
func (db *DB) RemoveIndexes(ctx context.Context, pieceCid cid.Cid, recordCount int) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.remove_indexes")
	defer span.End()

	// Get the number of shards
	_, totalShards := getShardPrefixBitCount(recordCount)
	for i := 0; i < totalShards; i++ {
		// Get the map of multihash -> offset/size for the shard
		shardPrefix, err := getShardPrefix(i)
		if err != nil {
			return err
		}
		cbKey := toCouchKey(pieceCid.String() + shardPrefix)
		cbMap := db.pieceOffsets.Map(cbKey)
		recMap, err := cbMap.Iterator()
		if err != nil {
			if isNotFoundErr(err) {
				// If there are no records in a particular shard just skip the shard
				continue
			}
			return fmt.Errorf("getting all records for piece %s: %w", pieceCid, err)
		}

		// Get each value in the map
		for mhStr := range recMap {
			cbKey := toCouchKey(mhStr)
			err = db.withCasRetry("remove-piece", func() error {
				getResult, err := db.mhToPieces.Get(cbKey, &gocb.GetOptions{Context: ctx})
				if err != nil {
					if isNotFoundErr(err) {
						return nil
					}
					return err
				}
				var cidStrs []string
				err = getResult.Content(&cidStrs)
				if err != nil {
					return err
				}

				// Remove multihash -> pieceCId (value only)
				for i, v := range cidStrs {
					if v == pieceCid.String() {
						cidStrs[i] = cidStrs[len(cidStrs)-1]
						cidStrs[len(cidStrs)-1] = ""
						cidStrs = cidStrs[:len(cidStrs)-1]
					}
				}

				_, err = db.mhToPieces.Replace(cbKey, cidStrs, &gocb.ReplaceOptions{
					Context: ctx,
					Cas:     getResult.Cas(),
				})
				if err != nil {
					return fmt.Errorf("removing multihash %s to piece %s: update doc: %w", mhStr, pieceCid, err)
				}
				return nil
			})
			if err != nil {
				return err
			}
			_, err = db.pieceOffsets.Remove(cbKey, &gocb.RemoveOptions{
				Context: ctx,
			})
			if err != nil {
				if isNotFoundErr(err) {
					return nil
				}
				return err
			}
		}
	}
	return nil
}

func (db *DB) RemoveDealForPiece(ctx context.Context, dealId string, pieceCid cid.Cid) error {
	ctx, span := tracing.Tracer.Start(ctx, "db.remove_deal_for_piece")
	defer span.End()

	return db.withCasRetry("remove-deal-for-piece", func() error {
		var getResult *gocb.GetResult
		k := toCouchKey(pieceCid.String())
		getResult, err := db.pcidToMeta.Get(k, &gocb.GetOptions{Context: ctx})
		if err != nil {
			if isNotFoundErr(err) {
				return nil
			}
			return fmt.Errorf("getting piece cid to metadata for piece %s: %w", pieceCid, err)
		}

		var metadata CouchbaseMetadata
		err = getResult.Content(&metadata)
		if err != nil {
			return fmt.Errorf("getting piece cid to metadata content for piece %s: %w", pieceCid, err)
		}

		for i, v := range metadata.Deals {
			if v.DealUuid == dealId {
				metadata.Deals[i] = metadata.Deals[len(metadata.Deals)-1]
				metadata.Deals = metadata.Deals[:len(metadata.Deals)-1]
				break
			}
		}
		// Remove Metadata if removed deal was last one
		if len(metadata.Deals) == 0 {
			if err := db.RemovePieceMetadata(ctx, pieceCid); err != nil {
				fmt.Errorf("Failed to remove the Metadata after removing the last deal: %w", err)
			}
			return nil
		}

		_, err = db.pcidToMeta.Replace(k, metadata, &gocb.ReplaceOptions{
			Context: ctx,
			Cas:     getResult.Cas(),
		})
		if err != nil {
			return fmt.Errorf("setting piece %s metadata: %w", pieceCid, err)
		}

		return nil
	})
}
