package channeldb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/wire"
	"github.com/coreos/bbolt"
	"github.com/go-errors/errors"
	"github.com/lightningnetwork/lnd/channeldb/migration_01_to_11"
	"github.com/lightningnetwork/lnd/lnwire"
)

const (
	dbName           = "channel.db"
	dbFilePermission = 0600
)

// migration is a function which takes a prior outdated version of the database
// instances and mutates the key/bucket structure to arrive at a more
// up-to-date version of the database.
type migration func(tx *bbolt.Tx) error

type version struct {
	number    uint32
	migration migration
}

var (
	// dbVersions is storing all versions of database. If current version
	// of database don't match with latest version this list will be used
	// for retrieving all migration function that are need to apply to the
	// current db.
	dbVersions = []version{
		{
			// The base DB version requires no migration.
			number:    0,
			migration: nil,
		},
		{
			// The version of the database where two new indexes
			// for the update time of node and channel updates were
			// added.
			number:    1,
			migration: migration_01_to_11.MigrateNodeAndEdgeUpdateIndex,
		},
		{
			// The DB version that added the invoice event time
			// series.
			number:    2,
			migration: migration_01_to_11.MigrateInvoiceTimeSeries,
		},
		{
			// The DB version that updated the embedded invoice in
			// outgoing payments to match the new format.
			number:    3,
			migration: migration_01_to_11.MigrateInvoiceTimeSeriesOutgoingPayments,
		},
		{
			// The version of the database where every channel
			// always has two entries in the edges bucket. If
			// a policy is unknown, this will be represented
			// by a special byte sequence.
			number:    4,
			migration: migration_01_to_11.MigrateEdgePolicies,
		},
		{
			// The DB version where we persist each attempt to send
			// an HTLC to a payment hash, and track whether the
			// payment is in-flight, succeeded, or failed.
			number:    5,
			migration: migration_01_to_11.PaymentStatusesMigration,
		},
		{
			// The DB version that properly prunes stale entries
			// from the edge update index.
			number:    6,
			migration: migration_01_to_11.MigratePruneEdgeUpdateIndex,
		},
		{
			// The DB version that migrates the ChannelCloseSummary
			// to a format where optional fields are indicated with
			// boolean flags.
			number:    7,
			migration: migration_01_to_11.MigrateOptionalChannelCloseSummaryFields,
		},
		{
			// The DB version that changes the gossiper's message
			// store keys to account for the message's type and
			// ShortChannelID.
			number:    8,
			migration: migration_01_to_11.MigrateGossipMessageStoreKeys,
		},
		{
			// The DB version where the payments and payment
			// statuses are moved to being stored in a combined
			// bucket.
			number:    9,
			migration: migration_01_to_11.MigrateOutgoingPayments,
		},
		{
			// The DB version where we started to store legacy
			// payload information for all routes, as well as the
			// optional TLV records.
			number:    10,
			migration: migration_01_to_11.MigrateRouteSerialization,
		},
		{
			// Add invoice htlc and cltv delta fields.
			number:    11,
			migration: migration_01_to_11.MigrateInvoices,
		},
	}

	// Big endian is the preferred byte order, due to cursor scans over
	// integer keys iterating in order.
	byteOrder = binary.BigEndian
)

// DB is the primary datastore for the lnd daemon. The database stores
// information related to nodes, routing data, open/closed channels, fee
// schedules, and reputation data.
type DB struct {
	*bbolt.DB
	dbPath string
	graph  *ChannelGraph
	now    func() time.Time
}

// Open opens an existing channeldb. Any necessary schemas migrations due to
// updates will take place as necessary.
func Open(dbPath string, modifiers ...OptionModifier) (*DB, error) {
	path := filepath.Join(dbPath, dbName)

	if !fileExists(path) {
		if err := createChannelDB(dbPath); err != nil {
			return nil, err
		}
	}

	opts := DefaultOptions()
	for _, modifier := range modifiers {
		modifier(&opts)
	}

	// Specify bbolt freelist options to reduce heap pressure in case the
	// freelist grows to be very large.
	options := &bbolt.Options{
		NoFreelistSync: opts.NoFreelistSync,
		FreelistType:   bbolt.FreelistMapType,
	}

	bdb, err := bbolt.Open(path, dbFilePermission, options)
	if err != nil {
		return nil, err
	}

	chanDB := &DB{
		DB:     bdb,
		dbPath: dbPath,
		now:    time.Now,
	}
	chanDB.graph = newChannelGraph(
		chanDB, opts.RejectCacheSize, opts.ChannelCacheSize,
	)

	// Synchronize the version of database and apply migrations if needed.
	if err := chanDB.syncVersions(dbVersions); err != nil {
		bdb.Close()
		return nil, err
	}

	return chanDB, nil
}

// Path returns the file path to the channel database.
func (d *DB) Path() string {
	return d.dbPath
}

// Wipe completely deletes all saved state within all used buckets within the
// database. The deletion is done in a single transaction, therefore this
// operation is fully atomic.
func (d *DB) Wipe() error {
	return d.Update(func(tx *bbolt.Tx) error {
		err := tx.DeleteBucket(openChannelBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(closedChannelBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(invoiceBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(nodeInfoBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		err = tx.DeleteBucket(nodeBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		err = tx.DeleteBucket(edgeBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		err = tx.DeleteBucket(edgeIndexBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		err = tx.DeleteBucket(graphMetaBucket)
		if err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}

		return nil
	})
}

// createChannelDB creates and initializes a fresh version of channeldb. In
// the case that the target path has not yet been created or doesn't yet exist,
// then the path is created. Additionally, all required top-level buckets used
// within the database are created.
func createChannelDB(dbPath string) error {
	if !fileExists(dbPath) {
		if err := os.MkdirAll(dbPath, 0700); err != nil {
			return err
		}
	}

	path := filepath.Join(dbPath, dbName)
	bdb, err := bbolt.Open(path, dbFilePermission, nil)
	if err != nil {
		return err
	}

	err = bdb.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucket(openChannelBucket); err != nil {
			return err
		}
		if _, err := tx.CreateBucket(closedChannelBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(forwardingLogBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(fwdPackagesKey); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(invoiceBucket); err != nil {
			return err
		}

		if _, err := tx.CreateBucket(nodeInfoBucket); err != nil {
			return err
		}

		nodes, err := tx.CreateBucket(nodeBucket)
		if err != nil {
			return err
		}
		_, err = nodes.CreateBucket(aliasIndexBucket)
		if err != nil {
			return err
		}
		_, err = nodes.CreateBucket(nodeUpdateIndexBucket)
		if err != nil {
			return err
		}

		edges, err := tx.CreateBucket(edgeBucket)
		if err != nil {
			return err
		}
		if _, err := edges.CreateBucket(edgeIndexBucket); err != nil {
			return err
		}
		if _, err := edges.CreateBucket(edgeUpdateIndexBucket); err != nil {
			return err
		}
		if _, err := edges.CreateBucket(channelPointBucket); err != nil {
			return err
		}
		if _, err := edges.CreateBucket(zombieBucket); err != nil {
			return err
		}

		graphMeta, err := tx.CreateBucket(graphMetaBucket)
		if err != nil {
			return err
		}
		_, err = graphMeta.CreateBucket(pruneLogBucket)
		if err != nil {
			return err
		}

		if _, err := tx.CreateBucket(metaBucket); err != nil {
			return err
		}

		meta := &Meta{
			DbVersionNumber: getLatestDBVersion(dbVersions),
		}
		return putMeta(meta, tx)
	})
	if err != nil {
		return fmt.Errorf("unable to create new channeldb")
	}

	return bdb.Close()
}

// fileExists returns true if the file exists, and false otherwise.
func fileExists(path string) bool {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false
		}
	}

	return true
}

// FetchOpenChannels starts a new database transaction and returns all stored
// currently active/open channels associated with the target nodeID. In the case
// that no active channels are known to have been created with this node, then a
// zero-length slice is returned.
func (d *DB) FetchOpenChannels(nodeID *btcec.PublicKey) ([]*OpenChannel, error) {
	var channels []*OpenChannel
	err := d.View(func(tx *bbolt.Tx) error {
		var err error
		channels, err = d.fetchOpenChannels(tx, nodeID)
		return err
	})

	return channels, err
}

// fetchOpenChannels uses and existing database transaction and returns all
// stored currently active/open channels associated with the target nodeID. In
// the case that no active channels are known to have been created with this
// node, then a zero-length slice is returned.
func (d *DB) fetchOpenChannels(tx *bbolt.Tx,
	nodeID *btcec.PublicKey) ([]*OpenChannel, error) {

	// Get the bucket dedicated to storing the metadata for open channels.
	openChanBucket := tx.Bucket(openChannelBucket)
	if openChanBucket == nil {
		return nil, nil
	}

	// Within this top level bucket, fetch the bucket dedicated to storing
	// open channel data specific to the remote node.
	pub := nodeID.SerializeCompressed()
	nodeChanBucket := openChanBucket.Bucket(pub)
	if nodeChanBucket == nil {
		return nil, nil
	}

	// Next, we'll need to go down an additional layer in order to retrieve
	// the channels for each chain the node knows of.
	var channels []*OpenChannel
	err := nodeChanBucket.ForEach(func(chainHash, v []byte) error {
		// If there's a value, it's not a bucket so ignore it.
		if v != nil {
			return nil
		}

		// If we've found a valid chainhash bucket, then we'll retrieve
		// that so we can extract all the channels.
		chainBucket := nodeChanBucket.Bucket(chainHash)
		if chainBucket == nil {
			return fmt.Errorf("unable to read bucket for chain=%x",
				chainHash[:])
		}

		// Finally, we both of the necessary buckets retrieved, fetch
		// all the active channels related to this node.
		nodeChannels, err := d.fetchNodeChannels(chainBucket)
		if err != nil {
			return fmt.Errorf("unable to read channel for "+
				"chain_hash=%x, node_key=%x: %v",
				chainHash[:], pub, err)
		}

		channels = append(channels, nodeChannels...)
		return nil
	})

	return channels, err
}

// fetchNodeChannels retrieves all active channels from the target chainBucket
// which is under a node's dedicated channel bucket. This function is typically
// used to fetch all the active channels related to a particular node.
func (d *DB) fetchNodeChannels(chainBucket *bbolt.Bucket) ([]*OpenChannel, error) {

	var channels []*OpenChannel

	// A node may have channels on several chains, so for each known chain,
	// we'll extract all the channels.
	err := chainBucket.ForEach(func(chanPoint, v []byte) error {
		// If there's a value, it's not a bucket so ignore it.
		if v != nil {
			return nil
		}

		// Once we've found a valid channel bucket, we'll extract it
		// from the node's chain bucket.
		chanBucket := chainBucket.Bucket(chanPoint)

		var outPoint wire.OutPoint
		err := readOutpoint(bytes.NewReader(chanPoint), &outPoint)
		if err != nil {
			return err
		}
		oChannel, err := fetchOpenChannel(chanBucket, &outPoint)
		if err != nil {
			return fmt.Errorf("unable to read channel data for "+
				"chan_point=%v: %v", outPoint, err)
		}
		oChannel.Db = d

		channels = append(channels, oChannel)

		return nil
	})
	if err != nil {
		return nil, err
	}

	return channels, nil
}

// FetchChannel attempts to locate a channel specified by the passed channel
// point. If the channel cannot be found, then an error will be returned.
func (d *DB) FetchChannel(chanPoint wire.OutPoint) (*OpenChannel, error) {
	var (
		targetChan      *OpenChannel
		targetChanPoint bytes.Buffer
	)

	if err := writeOutpoint(&targetChanPoint, &chanPoint); err != nil {
		return nil, err
	}

	// chanScan will traverse the following bucket structure:
	//  * nodePub => chainHash => chanPoint
	//
	// At each level we go one further, ensuring that we're traversing the
	// proper key (that's actually a bucket). By only reading the bucket
	// structure and skipping fully decoding each channel, we save a good
	// bit of CPU as we don't need to do things like decompress public
	// keys.
	chanScan := func(tx *bbolt.Tx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Within the node channel bucket, are the set of node pubkeys
		// we have channels with, we don't know the entire set, so
		// we'll check them all.
		return openChanBucket.ForEach(func(nodePub, v []byte) error {
			// Ensure that this is a key the same size as a pubkey,
			// and also that it leads directly to a bucket.
			if len(nodePub) != 33 || v != nil {
				return nil
			}

			nodeChanBucket := openChanBucket.Bucket(nodePub)
			if nodeChanBucket == nil {
				return nil
			}

			// The next layer down is all the chains that this node
			// has channels on with us.
			return nodeChanBucket.ForEach(func(chainHash, v []byte) error {
				// If there's a value, it's not a bucket so
				// ignore it.
				if v != nil {
					return nil
				}

				chainBucket := nodeChanBucket.Bucket(chainHash)
				if chainBucket == nil {
					return fmt.Errorf("unable to read "+
						"bucket for chain=%x", chainHash[:])
				}

				// Finally we reach the leaf bucket that stores
				// all the chanPoints for this node.
				chanBucket := chainBucket.Bucket(
					targetChanPoint.Bytes(),
				)
				if chanBucket == nil {
					return nil
				}

				channel, err := fetchOpenChannel(
					chanBucket, &chanPoint,
				)
				if err != nil {
					return err
				}

				targetChan = channel
				targetChan.Db = d

				return nil
			})
		})
	}

	err := d.View(chanScan)
	if err != nil {
		return nil, err
	}

	if targetChan != nil {
		return targetChan, nil
	}

	// If we can't find the channel, then we return with an error, as we
	// have nothing to  backup.
	return nil, ErrChannelNotFound
}

// FetchAllChannels attempts to retrieve all open channels currently stored
// within the database, including pending open, fully open and channels waiting
// for a closing transaction to confirm.
func (d *DB) FetchAllChannels() ([]*OpenChannel, error) {
	var channels []*OpenChannel

	// TODO(halseth): fetch all in one db tx.
	openChannels, err := d.FetchAllOpenChannels()
	if err != nil {
		return nil, err
	}
	channels = append(channels, openChannels...)

	pendingChannels, err := d.FetchPendingChannels()
	if err != nil {
		return nil, err
	}
	channels = append(channels, pendingChannels...)

	waitingClose, err := d.FetchWaitingCloseChannels()
	if err != nil {
		return nil, err
	}
	channels = append(channels, waitingClose...)

	return channels, nil
}

// FetchAllOpenChannels will return all channels that have the funding
// transaction confirmed, and is not waiting for a closing transaction to be
// confirmed.
func (d *DB) FetchAllOpenChannels() ([]*OpenChannel, error) {
	return fetchChannels(d, false, false)
}

// FetchPendingChannels will return channels that have completed the process of
// generating and broadcasting funding transactions, but whose funding
// transactions have yet to be confirmed on the blockchain.
func (d *DB) FetchPendingChannels() ([]*OpenChannel, error) {
	return fetchChannels(d, true, false)
}

// FetchWaitingCloseChannels will return all channels that have been opened,
// but are now waiting for a closing transaction to be confirmed.
//
// NOTE: This includes channels that are also pending to be opened.
func (d *DB) FetchWaitingCloseChannels() ([]*OpenChannel, error) {
	waitingClose, err := fetchChannels(d, false, true)
	if err != nil {
		return nil, err
	}
	pendingWaitingClose, err := fetchChannels(d, true, true)
	if err != nil {
		return nil, err
	}

	return append(waitingClose, pendingWaitingClose...), nil
}

// fetchChannels attempts to retrieve channels currently stored in the
// database. The pending parameter determines whether only pending channels
// will be returned, or only open channels will be returned. The waitingClose
// parameter determines whether only channels waiting for a closing transaction
// to be confirmed should be returned. If no active channels exist within the
// network, then ErrNoActiveChannels is returned.
func fetchChannels(d *DB, pending, waitingClose bool) ([]*OpenChannel, error) {
	var channels []*OpenChannel

	err := d.View(func(tx *bbolt.Tx) error {
		// Get the bucket dedicated to storing the metadata for open
		// channels.
		openChanBucket := tx.Bucket(openChannelBucket)
		if openChanBucket == nil {
			return ErrNoActiveChannels
		}

		// Next, fetch the bucket dedicated to storing metadata related
		// to all nodes. All keys within this bucket are the serialized
		// public keys of all our direct counterparties.
		nodeMetaBucket := tx.Bucket(nodeInfoBucket)
		if nodeMetaBucket == nil {
			return fmt.Errorf("node bucket not created")
		}

		// Finally for each node public key in the bucket, fetch all
		// the channels related to this particular node.
		return nodeMetaBucket.ForEach(func(k, v []byte) error {
			nodeChanBucket := openChanBucket.Bucket(k)
			if nodeChanBucket == nil {
				return nil
			}

			return nodeChanBucket.ForEach(func(chainHash, v []byte) error {
				// If there's a value, it's not a bucket so
				// ignore it.
				if v != nil {
					return nil
				}

				// If we've found a valid chainhash bucket,
				// then we'll retrieve that so we can extract
				// all the channels.
				chainBucket := nodeChanBucket.Bucket(chainHash)
				if chainBucket == nil {
					return fmt.Errorf("unable to read "+
						"bucket for chain=%x", chainHash[:])
				}

				nodeChans, err := d.fetchNodeChannels(chainBucket)
				if err != nil {
					return fmt.Errorf("unable to read "+
						"channel for chain_hash=%x, "+
						"node_key=%x: %v", chainHash[:], k, err)
				}
				for _, channel := range nodeChans {
					if channel.IsPending != pending {
						continue
					}

					// If the channel is in any other state
					// than Default, then it means it is
					// waiting to be closed.
					channelWaitingClose :=
						channel.ChanStatus() != ChanStatusDefault

					// Only include it if we requested
					// channels with the same waitingClose
					// status.
					if channelWaitingClose != waitingClose {
						continue
					}

					channels = append(channels, channel)
				}
				return nil
			})

		})
	})
	if err != nil {
		return nil, err
	}

	return channels, nil
}

// FetchClosedChannels attempts to fetch all closed channels from the database.
// The pendingOnly bool toggles if channels that aren't yet fully closed should
// be returned in the response or not. When a channel was cooperatively closed,
// it becomes fully closed after a single confirmation.  When a channel was
// forcibly closed, it will become fully closed after _all_ the pending funds
// (if any) have been swept.
func (d *DB) FetchClosedChannels(pendingOnly bool) ([]*ChannelCloseSummary, error) {
	var chanSummaries []*ChannelCloseSummary

	if err := d.View(func(tx *bbolt.Tx) error {
		closeBucket := tx.Bucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrNoClosedChannels
		}

		return closeBucket.ForEach(func(chanID []byte, summaryBytes []byte) error {
			summaryReader := bytes.NewReader(summaryBytes)
			chanSummary, err := deserializeCloseChannelSummary(summaryReader)
			if err != nil {
				return err
			}

			// If the query specified to only include pending
			// channels, then we'll skip any channels which aren't
			// currently pending.
			if !chanSummary.IsPending && pendingOnly {
				return nil
			}

			chanSummaries = append(chanSummaries, chanSummary)
			return nil
		})
	}); err != nil {
		return nil, err
	}

	return chanSummaries, nil
}

// ErrClosedChannelNotFound signals that a closed channel could not be found in
// the channeldb.
var ErrClosedChannelNotFound = errors.New("unable to find closed channel summary")

// FetchClosedChannel queries for a channel close summary using the channel
// point of the channel in question.
func (d *DB) FetchClosedChannel(chanID *wire.OutPoint) (*ChannelCloseSummary, error) {
	var chanSummary *ChannelCloseSummary
	if err := d.View(func(tx *bbolt.Tx) error {
		closeBucket := tx.Bucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrClosedChannelNotFound
		}

		var b bytes.Buffer
		var err error
		if err = writeOutpoint(&b, chanID); err != nil {
			return err
		}

		summaryBytes := closeBucket.Get(b.Bytes())
		if summaryBytes == nil {
			return ErrClosedChannelNotFound
		}

		summaryReader := bytes.NewReader(summaryBytes)
		chanSummary, err = deserializeCloseChannelSummary(summaryReader)

		return err
	}); err != nil {
		return nil, err
	}

	return chanSummary, nil
}

// FetchClosedChannelForID queries for a channel close summary using the
// channel ID of the channel in question.
func (d *DB) FetchClosedChannelForID(cid lnwire.ChannelID) (
	*ChannelCloseSummary, error) {

	var chanSummary *ChannelCloseSummary
	if err := d.View(func(tx *bbolt.Tx) error {
		closeBucket := tx.Bucket(closedChannelBucket)
		if closeBucket == nil {
			return ErrClosedChannelNotFound
		}

		// The first 30 bytes of the channel ID and outpoint will be
		// equal.
		cursor := closeBucket.Cursor()
		op, c := cursor.Seek(cid[:30])

		// We scan over all possible candidates for this channel ID.
		for ; op != nil && bytes.Compare(cid[:30], op[:30]) <= 0; op, c = cursor.Next() {
			var outPoint wire.OutPoint
			err := readOutpoint(bytes.NewReader(op), &outPoint)
			if err != nil {
				return err
			}

			// If the found outpoint does not correspond to this
			// channel ID, we continue.
			if !cid.IsChanPoint(&outPoint) {
				continue
			}

			// Deserialize the close summary and return.
			r := bytes.NewReader(c)
			chanSummary, err = deserializeCloseChannelSummary(r)
			if err != nil {
				return err
			}

			return nil
		}
		return ErrClosedChannelNotFound
	}); err != nil {
		return nil, err
	}

	return chanSummary, nil
}

// MarkChanFullyClosed marks a channel as fully closed within the database. A
// channel should be marked as fully closed if the channel was initially
// cooperatively closed and it's reached a single confirmation, or after all
// the pending funds in a channel that has been forcibly closed have been
// swept.
func (d *DB) MarkChanFullyClosed(chanPoint *wire.OutPoint) error {
	return d.Update(func(tx *bbolt.Tx) error {
		var b bytes.Buffer
		if err := writeOutpoint(&b, chanPoint); err != nil {
			return err
		}

		chanID := b.Bytes()

		closedChanBucket, err := tx.CreateBucketIfNotExists(
			closedChannelBucket,
		)
		if err != nil {
			return err
		}

		chanSummaryBytes := closedChanBucket.Get(chanID)
		if chanSummaryBytes == nil {
			return fmt.Errorf("no closed channel for "+
				"chan_point=%v found", chanPoint)
		}

		chanSummaryReader := bytes.NewReader(chanSummaryBytes)
		chanSummary, err := deserializeCloseChannelSummary(
			chanSummaryReader,
		)
		if err != nil {
			return err
		}

		chanSummary.IsPending = false

		var newSummary bytes.Buffer
		err = serializeChannelCloseSummary(&newSummary, chanSummary)
		if err != nil {
			return err
		}

		err = closedChanBucket.Put(chanID, newSummary.Bytes())
		if err != nil {
			return err
		}

		// Now that the channel is closed, we'll check if we have any
		// other open channels with this peer. If we don't we'll
		// garbage collect it to ensure we don't establish persistent
		// connections to peers without open channels.
		return d.pruneLinkNode(tx, chanSummary.RemotePub)
	})
}

// pruneLinkNode determines whether we should garbage collect a link node from
// the database due to no longer having any open channels with it. If there are
// any left, then this acts as a no-op.
func (d *DB) pruneLinkNode(tx *bbolt.Tx, remotePub *btcec.PublicKey) error {
	openChannels, err := d.fetchOpenChannels(tx, remotePub)
	if err != nil {
		return fmt.Errorf("unable to fetch open channels for peer %x: "+
			"%v", remotePub.SerializeCompressed(), err)
	}

	if len(openChannels) > 0 {
		return nil
	}

	log.Infof("Pruning link node %x with zero open channels from database",
		remotePub.SerializeCompressed())

	return d.deleteLinkNode(tx, remotePub)
}

// PruneLinkNodes attempts to prune all link nodes found within the databse with
// whom we no longer have any open channels with.
func (d *DB) PruneLinkNodes() error {
	return d.Update(func(tx *bbolt.Tx) error {
		linkNodes, err := d.fetchAllLinkNodes(tx)
		if err != nil {
			return err
		}

		for _, linkNode := range linkNodes {
			err := d.pruneLinkNode(tx, linkNode.IdentityPub)
			if err != nil {
				return err
			}
		}

		return nil
	})
}

// ChannelShell is a shell of a channel that is meant to be used for channel
// recovery purposes. It contains a minimal OpenChannel instance along with
// addresses for that target node.
type ChannelShell struct {
	// NodeAddrs the set of addresses that this node has known to be
	// reachable at in the past.
	NodeAddrs []net.Addr

	// Chan is a shell of an OpenChannel, it contains only the items
	// required to restore the channel on disk.
	Chan *OpenChannel
}

// RestoreChannelShells is a method that allows the caller to reconstruct the
// state of an OpenChannel from the ChannelShell. We'll attempt to write the
// new channel to disk, create a LinkNode instance with the passed node
// addresses, and finally create an edge within the graph for the channel as
// well. This method is idempotent, so repeated calls with the same set of
// channel shells won't modify the database after the initial call.
func (d *DB) RestoreChannelShells(channelShells ...*ChannelShell) error {
	chanGraph := d.ChannelGraph()

	// TODO(conner): find way to do this w/o accessing internal members?
	chanGraph.cacheMu.Lock()
	defer chanGraph.cacheMu.Unlock()

	var chansRestored []uint64
	err := d.Update(func(tx *bbolt.Tx) error {
		for _, channelShell := range channelShells {
			channel := channelShell.Chan

			// When we make a channel, we mark that the channel has
			// been restored, this will signal to other sub-systems
			// to not attempt to use the channel as if it was a
			// regular one.
			channel.chanStatus |= ChanStatusRestored

			// First, we'll attempt to create a new open channel
			// and link node for this channel. If the channel
			// already exists, then in order to ensure this method
			// is idempotent, we'll continue to the next step.
			channel.Db = d
			err := syncNewChannel(
				tx, channel, channelShell.NodeAddrs,
			)
			if err != nil {
				return err
			}

			// Next, we'll create an active edge in the graph
			// database for this channel in order to restore our
			// partial view of the network.
			//
			// TODO(roasbeef): if we restore *after* the channel
			// has been closed on chain, then need to inform the
			// router that it should try and prune these values as
			// we can detect them
			edgeInfo := ChannelEdgeInfo{
				ChannelID:    channel.ShortChannelID.ToUint64(),
				ChainHash:    channel.ChainHash,
				ChannelPoint: channel.FundingOutpoint,
				Capacity:     channel.Capacity,
			}

			nodes := tx.Bucket(nodeBucket)
			if nodes == nil {
				return ErrGraphNotFound
			}
			selfNode, err := chanGraph.sourceNode(nodes)
			if err != nil {
				return err
			}

			// Depending on which pub key is smaller, we'll assign
			// our roles as "node1" and "node2".
			chanPeer := channel.IdentityPub.SerializeCompressed()
			selfIsSmaller := bytes.Compare(
				selfNode.PubKeyBytes[:], chanPeer,
			) == -1
			if selfIsSmaller {
				copy(edgeInfo.NodeKey1Bytes[:], selfNode.PubKeyBytes[:])
				copy(edgeInfo.NodeKey2Bytes[:], chanPeer)
			} else {
				copy(edgeInfo.NodeKey1Bytes[:], chanPeer)
				copy(edgeInfo.NodeKey2Bytes[:], selfNode.PubKeyBytes[:])
			}

			// With the edge info shell constructed, we'll now add
			// it to the graph.
			err = chanGraph.addChannelEdge(tx, &edgeInfo)
			if err != nil && err != ErrEdgeAlreadyExist {
				return err
			}

			// Similarly, we'll construct a channel edge shell and
			// add that itself to the graph.
			chanEdge := ChannelEdgePolicy{
				ChannelID:  edgeInfo.ChannelID,
				LastUpdate: time.Now(),
			}

			// If their pubkey is larger, then we'll flip the
			// direction bit to indicate that us, the "second" node
			// is updating their policy.
			if !selfIsSmaller {
				chanEdge.ChannelFlags |= lnwire.ChanUpdateDirection
			}

			_, err = updateEdgePolicy(tx, &chanEdge)
			if err != nil {
				return err
			}

			chansRestored = append(chansRestored, edgeInfo.ChannelID)
		}

		return nil
	})
	if err != nil {
		return err
	}

	for _, chanid := range chansRestored {
		chanGraph.rejectCache.remove(chanid)
		chanGraph.chanCache.remove(chanid)
	}

	return nil
}

// AddrsForNode consults the graph and channel database for all addresses known
// to the passed node public key.
func (d *DB) AddrsForNode(nodePub *btcec.PublicKey) ([]net.Addr, error) {
	var (
		linkNode  *LinkNode
		graphNode LightningNode
	)

	dbErr := d.View(func(tx *bbolt.Tx) error {
		var err error

		linkNode, err = fetchLinkNode(tx, nodePub)
		if err != nil {
			return err
		}

		// We'll also query the graph for this peer to see if they have
		// any addresses that we don't currently have stored within the
		// link node database.
		nodes := tx.Bucket(nodeBucket)
		if nodes == nil {
			return ErrGraphNotFound
		}
		compressedPubKey := nodePub.SerializeCompressed()
		graphNode, err = fetchLightningNode(nodes, compressedPubKey)
		if err != nil && err != ErrGraphNodeNotFound {
			// If the node isn't found, then that's OK, as we still
			// have the link node data.
			return err
		}

		return nil
	})
	if dbErr != nil {
		return nil, dbErr
	}

	// Now that we have both sources of addrs for this node, we'll use a
	// map to de-duplicate any addresses between the two sources, and
	// produce a final list of the combined addrs.
	addrs := make(map[string]net.Addr)
	for _, addr := range linkNode.Addresses {
		addrs[addr.String()] = addr
	}
	for _, addr := range graphNode.Addresses {
		addrs[addr.String()] = addr
	}
	dedupedAddrs := make([]net.Addr, 0, len(addrs))
	for _, addr := range addrs {
		dedupedAddrs = append(dedupedAddrs, addr)
	}

	return dedupedAddrs, nil
}

// AbandonChannel attempts to remove the target channel from the open channel
// database. If the channel was already removed (has a closed channel entry),
// then we'll return a nil error. Otherwise, we'll insert a new close summary
// into the database.
func (d *DB) AbandonChannel(chanPoint *wire.OutPoint, bestHeight uint32) error {
	// With the chanPoint constructed, we'll attempt to find the target
	// channel in the database. If we can't find the channel, then we'll
	// return the error back to the caller.
	dbChan, err := d.FetchChannel(*chanPoint)
	switch {
	// If the channel wasn't found, then it's possible that it was already
	// abandoned from the database.
	case err == ErrChannelNotFound:
		_, closedErr := d.FetchClosedChannel(chanPoint)
		if closedErr != nil {
			return closedErr
		}

		// If the channel was already closed, then we don't return an
		// error as we'd like fro this step to be repeatable.
		return nil
	case err != nil:
		return err
	}

	// Now that we've found the channel, we'll populate a close summary for
	// the channel, so we can store as much information for this abounded
	// channel as possible. We also ensure that we set Pending to false, to
	// indicate that this channel has been "fully" closed.
	summary := &ChannelCloseSummary{
		CloseType:               Abandoned,
		ChanPoint:               *chanPoint,
		ChainHash:               dbChan.ChainHash,
		CloseHeight:             bestHeight,
		RemotePub:               dbChan.IdentityPub,
		Capacity:                dbChan.Capacity,
		SettledBalance:          dbChan.LocalCommitment.LocalBalance.ToSatoshis(),
		ShortChanID:             dbChan.ShortChanID(),
		RemoteCurrentRevocation: dbChan.RemoteCurrentRevocation,
		RemoteNextRevocation:    dbChan.RemoteNextRevocation,
		LocalChanConfig:         dbChan.LocalChanCfg,
	}

	// Finally, we'll close the channel in the DB, and return back to the
	// caller.
	return dbChan.CloseChannel(summary)
}

// syncVersions function is used for safe db version synchronization. It
// applies migration functions to the current database and recovers the
// previous state of db if at least one error/panic appeared during migration.
func (d *DB) syncVersions(versions []version) error {
	meta, err := d.FetchMeta(nil)
	if err != nil {
		if err == ErrMetaNotFound {
			meta = &Meta{}
		} else {
			return err
		}
	}

	latestVersion := getLatestDBVersion(versions)
	log.Infof("Checking for schema update: latest_version=%v, "+
		"db_version=%v", latestVersion, meta.DbVersionNumber)

	switch {

	// If the database reports a higher version that we are aware of, the
	// user is probably trying to revert to a prior version of lnd. We fail
	// here to prevent reversions and unintended corruption.
	case meta.DbVersionNumber > latestVersion:
		log.Errorf("Refusing to revert from db_version=%d to "+
			"lower version=%d", meta.DbVersionNumber,
			latestVersion)
		return ErrDBReversion

	// If the current database version matches the latest version number,
	// then we don't need to perform any migrations.
	case meta.DbVersionNumber == latestVersion:
		return nil
	}

	log.Infof("Performing database schema migration")

	// Otherwise, we fetch the migrations which need to applied, and
	// execute them serially within a single database transaction to ensure
	// the migration is atomic.
	migrations, migrationVersions := getMigrationsToApply(
		versions, meta.DbVersionNumber,
	)
	return d.Update(func(tx *bbolt.Tx) error {
		for i, migration := range migrations {
			if migration == nil {
				continue
			}

			log.Infof("Applying migration #%v", migrationVersions[i])

			if err := migration(tx); err != nil {
				log.Infof("Unable to apply migration #%v",
					migrationVersions[i])
				return err
			}
		}

		meta.DbVersionNumber = latestVersion
		return putMeta(meta, tx)
	})
}

// ChannelGraph returns a new instance of the directed channel graph.
func (d *DB) ChannelGraph() *ChannelGraph {
	return d.graph
}

func getLatestDBVersion(versions []version) uint32 {
	return versions[len(versions)-1].number
}

// getMigrationsToApply retrieves the migration function that should be
// applied to the database.
func getMigrationsToApply(versions []version, version uint32) ([]migration, []uint32) {
	migrations := make([]migration, 0, len(versions))
	migrationVersions := make([]uint32, 0, len(versions))

	for _, v := range versions {
		if v.number > version {
			migrations = append(migrations, v.migration)
			migrationVersions = append(migrationVersions, v.number)
		}
	}

	return migrations, migrationVersions
}
