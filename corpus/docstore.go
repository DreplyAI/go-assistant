package corpus

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// DocStore is optional document-level storage.
//
// The vector index holds chunks, and a chunk browser answers "what exactly got
// embedded, and why did this one rank" — which the search admin already does
// well. It cannot answer "which documents do we actually have, from which
// source, under which licence, and is this corpus any good", because those are
// questions about documents and the index has no documents in it.
//
// Optional because a corpus is fully searchable without one. Registering a
// store adds the document browser; omitting it costs nothing else.
type DocStore interface {
	Put(ctx context.Context, corpus string, docs []Doc) error
	Get(ctx context.Context, corpus, id string) (*Doc, error)
	List(ctx context.Context, corpus string, q DocQuery) (DocPage, error)
	Count(ctx context.Context, corpus string) (int64, error)
}

// DocQuery filters the document browser.
type DocQuery struct {
	Search  string // substring on title
	Licence string
	Source  string
	Kind    string
	From    int // published-year range, inclusive; 0 means unbounded
	To      int

	Limit  int
	Offset int
}

// DocPage is one page of documents plus the total the filter matched — the
// admin needs both to render "1–50 of 189,341".
type DocPage struct {
	Docs  []Doc `json:"docs"`
	Total int64 `json:"total"`
}

var (
	storeMu sync.RWMutex
	stores  = map[string]DocStore{}
)

// RegisterDocStore attaches a store to a corpus.
func RegisterDocStore(corpus string, s DocStore) {
	storeMu.Lock()
	defer storeMu.Unlock()
	if s == nil {
		delete(stores, corpus)
		return
	}
	stores[corpus] = s
}

// Store returns the store for a corpus, if one was registered.
func Store(corpus string) (DocStore, bool) {
	storeMu.RLock()
	defer storeMu.RUnlock()
	s, ok := stores[corpus]
	return s, ok
}

// --- Mongo -------------------------------------------------------------

// ErrNoStore is returned by MongoStore when it was built without a database.
var ErrNoStore = errors.New("corpus: no document store")

// MongoStore is the default DocStore.
type MongoStore struct {
	coll *mongo.Collection
}

// NewMongoStore builds a store over one collection.
//
// One collection for every corpus, discriminated by a `corpus` field, rather
// than a collection each: corpora are declared at runtime by whichever modules
// happen to be imported, so a collection-per-corpus would mean creating
// collections and indexes from a registration callback. The discriminator also
// makes "how many documents across all corpora" a single query.
func NewMongoStore(ctx context.Context, db *mongo.Database, name string) (*MongoStore, error) {
	if db == nil {
		return nil, ErrNoStore
	}
	if name == "" {
		name = "corpus_documents"
	}
	s := &MongoStore{coll: db.Collection(name)}

	// (corpus, docId) is the identity, and it is unique: re-ingesting a
	// document must update it rather than accumulate copies. The rest support
	// the browser's filters — without them, filtering a six-figure corpus is a
	// collection scan per page.
	_, err := s.coll.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{
			Keys:    bson.D{{Key: "corpus", Value: 1}, {Key: "docId", Value: 1}},
			Options: options.Index().SetUnique(true).SetName("corpus_doc"),
		},
		{Keys: bson.D{{Key: "corpus", Value: 1}, {Key: "published", Value: -1}}},
		{Keys: bson.D{{Key: "corpus", Value: 1}, {Key: "licence", Value: 1}}},
		{Keys: bson.D{{Key: "corpus", Value: 1}, {Key: "prov.source", Value: 1}}},
		{Keys: bson.D{{Key: "corpus", Value: 1}, {Key: "title", Value: "text"}}},
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

// docRow is the stored shape. Kept separate from Doc because Doc is the
// pipeline's currency and should not grow bson tags and a corpus field for
// the sake of one optional consumer.
type docRow struct {
	Corpus    string         `bson:"corpus"`
	DocID     string         `bson:"docId"`
	Kind      string         `bson:"kind"`
	Title     string         `bson:"title"`
	Body      string         `bson:"body,omitempty"`
	URL       string         `bson:"url,omitempty"`
	Path      string         `bson:"path,omitempty"`
	Published string         `bson:"published,omitempty"`
	Container string         `bson:"container,omitempty"`
	Licence   string         `bson:"licence"`
	Extra     map[string]any `bson:"extra,omitempty"`
	Prov      Provenance     `bson:"prov"`
	UpdatedAt time.Time      `bson:"updatedAt"`
}

func (s *MongoStore) Put(ctx context.Context, corpus string, docs []Doc) error {
	if len(docs) == 0 {
		return nil
	}
	models := make([]mongo.WriteModel, 0, len(docs))
	now := time.Now().UTC()
	for _, d := range docs {
		r := docRow{
			Corpus: corpus, DocID: d.ID, Kind: d.Kind, Title: d.Title,
			Body: d.Body, URL: d.URL, Path: d.Path, Published: d.Published,
			Container: d.Container, Licence: licenceOr(d.Licence), Extra: d.Extra,
			Prov: d.Prov, UpdatedAt: now,
		}
		models = append(models, mongo.NewReplaceOneModel().
			SetFilter(bson.M{"corpus": corpus, "docId": d.ID}).
			SetReplacement(r).
			SetUpsert(true))
	}
	// Unordered: one malformed document should not stop the rest of a batch.
	_, err := s.coll.BulkWrite(ctx, models, options.BulkWrite().SetOrdered(false))
	return err
}

func (s *MongoStore) Get(ctx context.Context, corpus, id string) (*Doc, error) {
	var r docRow
	err := s.coll.FindOne(ctx, bson.M{"corpus": corpus, "docId": id}).Decode(&r)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	d := r.toDoc()
	return &d, nil
}

func (s *MongoStore) List(ctx context.Context, corpus string, q DocQuery) (DocPage, error) {
	filter := bson.M{"corpus": corpus}
	if q.Licence != "" {
		filter["licence"] = q.Licence
	}
	if q.Source != "" {
		filter["prov.source"] = q.Source
	}
	if q.Kind != "" {
		filter["kind"] = q.Kind
	}
	if q.Search != "" {
		// Anchored on a regex rather than the text index: an operator looking
		// for a specific paper types a fragment of its title, and $text only
		// matches whole words.
		filter["title"] = bson.M{"$regex": regexEscape(q.Search), "$options": "i"}
	}
	if q.From > 0 || q.To > 0 {
		// Published is a string ("2017", "2017-03-01"), so the range is
		// lexicographic — which is correct for both forms because they share a
		// year prefix.
		rng := bson.M{}
		if q.From > 0 {
			rng["$gte"] = itoa(q.From)
		}
		if q.To > 0 {
			rng["$lte"] = itoa(q.To) + "￿"
		}
		filter["published"] = rng
	}

	total, err := s.coll.CountDocuments(ctx, filter)
	if err != nil {
		return DocPage{}, err
	}

	limit := q.Limit
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	opts := options.Find().
		SetSort(bson.D{{Key: "published", Value: -1}, {Key: "docId", Value: 1}}).
		SetSkip(int64(q.Offset)).
		SetLimit(int64(limit)).
		// Body is the whole abstract or page; a 50-row listing does not need
		// it and pulling it makes every page an order of magnitude larger.
		SetProjection(bson.M{"body": 0})

	cur, err := s.coll.Find(ctx, filter, opts)
	if err != nil {
		return DocPage{}, err
	}
	defer cur.Close(ctx)

	page := DocPage{Total: total, Docs: []Doc{}}
	for cur.Next(ctx) {
		var r docRow
		if err := cur.Decode(&r); err != nil {
			return DocPage{}, err
		}
		page.Docs = append(page.Docs, r.toDoc())
	}
	return page, cur.Err()
}

func (s *MongoStore) Count(ctx context.Context, corpus string) (int64, error) {
	return s.coll.CountDocuments(ctx, bson.M{"corpus": corpus})
}

func (r docRow) toDoc() Doc {
	return Doc{
		ID: r.DocID, Kind: r.Kind, Title: r.Title, Body: r.Body,
		URL: r.URL, Path: r.Path, Published: r.Published,
		Container: r.Container, Licence: r.Licence, Extra: r.Extra, Prov: r.Prov,
	}
}

// licenceOr keeps the "recorded, never omitted" rule true at the storage
// boundary too — a row that reached here without one is unknown, not blank.
func licenceOr(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// regexEscape neutralises regex metacharacters in operator input. A title
// search containing "(" would otherwise be a syntax error rather than a search.
func regexEscape(s string) string {
	const special = `\.+*?()|[]{}^$`
	out := make([]byte, 0, len(s)+8)
	for i := range len(s) {
		if idx := indexByte(special, s[i]); idx >= 0 {
			out = append(out, '\\')
		}
		out = append(out, s[i])
	}
	return string(out)
}

func indexByte(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}
