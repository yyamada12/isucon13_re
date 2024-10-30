package main

// ISUCON的な参考: https://github.com/isucon/isucon12-qualify/blob/main/webapp/go/isuports.go#L336
// sqlx的な参考: https://jmoiron.github.io/sqlx/

import (
	"crypto/sha256"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"sync"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"golang.org/x/exp/constraints"

	"github.com/gorilla/sessions"
	"github.com/labstack/echo-contrib/session"
	echolog "github.com/labstack/gommon/log"

	_ "net/http/pprof"

	"github.com/felixge/fgprof"
)

const (
	listenPort                     = 8080
	powerDNSSubdomainAddressEnvKey = "ISUCON13_POWERDNS_SUBDOMAIN_ADDRESS"
)

var (
	powerDNSSubdomainAddress string
	dbConn                   *sqlx.DB
	secret                   = []byte("isucon13_session_cookiestore_defaultsecret")
)

func init() {
	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	if secretKey, ok := os.LookupEnv("ISUCON13_SESSION_SECRETKEY"); ok {
		secret = []byte(secretKey)
	}
}

type InitializeResponse struct {
	Language string `json:"language"`
}

func connectDB(logger echo.Logger) (*sqlx.DB, error) {
	const (
		networkTypeEnvKey = "ISUCON13_MYSQL_DIALCONFIG_NET"
		addrEnvKey        = "ISUCON13_MYSQL_DIALCONFIG_ADDRESS"
		portEnvKey        = "ISUCON13_MYSQL_DIALCONFIG_PORT"
		userEnvKey        = "ISUCON13_MYSQL_DIALCONFIG_USER"
		passwordEnvKey    = "ISUCON13_MYSQL_DIALCONFIG_PASSWORD"
		dbNameEnvKey      = "ISUCON13_MYSQL_DIALCONFIG_DATABASE"
		parseTimeEnvKey   = "ISUCON13_MYSQL_DIALCONFIG_PARSETIME"
	)

	conf := mysql.NewConfig()

	// 環境変数がセットされていなかった場合でも一旦動かせるように、デフォルト値を入れておく
	// この挙動を変更して、エラーを出すようにしてもいいかもしれない
	conf.Net = "tcp"
	conf.Addr = net.JoinHostPort("127.0.0.1", "3306")
	conf.User = "isucon"
	conf.Passwd = "isucon"
	conf.DBName = "isupipe"
	conf.ParseTime = true
	conf.InterpolateParams = true

	if v, ok := os.LookupEnv(networkTypeEnvKey); ok {
		conf.Net = v
	}
	if addr, ok := os.LookupEnv(addrEnvKey); ok {
		if port, ok2 := os.LookupEnv(portEnvKey); ok2 {
			conf.Addr = net.JoinHostPort(addr, port)
		} else {
			conf.Addr = net.JoinHostPort(addr, "3306")
		}
	}
	if v, ok := os.LookupEnv(userEnvKey); ok {
		conf.User = v
	}
	if v, ok := os.LookupEnv(passwordEnvKey); ok {
		conf.Passwd = v
	}
	if v, ok := os.LookupEnv(dbNameEnvKey); ok {
		conf.DBName = v
	}
	if v, ok := os.LookupEnv(parseTimeEnvKey); ok {
		parseTime, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("failed to parse environment variable '%s' as bool: %+v", parseTimeEnvKey, err)
		}
		conf.ParseTime = parseTime
	}

	db, err := sqlx.Open("mysql", conf.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(10)

	if err := db.Ping(); err != nil {
		return nil, err
	}

	return db, nil
}

func initializeHandler(c echo.Context) error {
	if out, err := exec.Command("../sql/init.sh").CombinedOutput(); err != nil {
		c.Logger().Warnf("init.sh failed with err=%s", string(out))
		return echo.NewHTTPError(http.StatusInternalServerError, "failed to initialize: "+err.Error())
	}

	LoadCache()

	go func() {
		if out, err := exec.Command("go", "tool", "pprof", "-seconds=30", "-proto", "-output", "/home/isucon/pprof/pprof.pb.gz", "localhost:6060/debug/pprof/profile").CombinedOutput(); err != nil {
			fmt.Printf("pprof failed with err=%s, %s", string(out), err)
		} else {
			fmt.Printf("pprof.pb.gz created: %s", string(out))
		}
	}()
	go func() {
		if out, err := exec.Command("go", "tool", "pprof", "-seconds=30", "-proto", "-output", "/home/isucon/pprof/fgprof.pb.gz", "localhost:6060/debug/fgprof").CombinedOutput(); err != nil {
			fmt.Printf("fgprof failed with err=%s, %s", string(out), err)
		} else {
			fmt.Printf("fgprof.pb.gz created: %s", string(out))
		}
	}()

	c.Request().Header.Add("Content-Type", "application/json;charset=utf-8")
	return c.JSON(http.StatusOK, InitializeResponse{
		Language: "golang",
	})
}

type SyncMap[K comparable, V any] struct {
	m  map[K]*V
	mu sync.RWMutex
}

func NewSyncMap[K comparable, V any]() *SyncMap[K, V] {
	return &SyncMap[K, V]{m: map[K]*V{}}
}

func (sm *SyncMap[K, V]) Add(key K, value V) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[key] = &value
}

func (sm *SyncMap[K, V]) Get(key K) *V {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.m[key]
}

func (sm *SyncMap[K, V]) Delete(key K) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	delete(sm.m, key)
}

func (sm *SyncMap[K, V]) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m = map[K]*V{}
}

type SyncCounterMap[K comparable, V constraints.Integer] struct {
	m  map[K]V
	mu sync.RWMutex
}

func NewSyncCounterMap[K comparable, V constraints.Integer]() *SyncCounterMap[K, V] {
	return &SyncCounterMap[K, V]{m: map[K]V{}}
}

func (sm *SyncCounterMap[K, V]) Add(key K, value V) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[key] += value
}

func (sm *SyncCounterMap[K, V]) Get(key K) V {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.m[key]
}

func (sm *SyncCounterMap[K, V]) Delete(key K) {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	delete(sm.m, key)
}

func (sm *SyncCounterMap[K, V]) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m = map[K]V{}
}

type SyncListMap[K comparable, V any] struct {
	m  map[K][]V
	mu sync.RWMutex
}

func NewSyncListMap[K comparable, V any]() *SyncListMap[K, V] {
	return &SyncListMap[K, V]{m: map[K][]V{}}
}

func (sm *SyncListMap[K, V]) Add(key K, value V) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m[key] = append(sm.m[key], value)
}

func (sm *SyncListMap[K, V]) Get(key K) []V {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.m[key]
}

func (sm *SyncListMap[K, V]) Clear() {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	sm.m = map[K][]V{}
}

type Icon struct {
	Image []byte
	Hash  [32]byte
}

var iconUsernameMap = NewSyncMap[string, Icon]()
var iconUserMap = NewSyncMap[int64, Icon]()
var themeMap = NewSyncMap[int64, ThemeModel]()
var userMap = NewSyncMap[int64, UserModel]()
var tagMap = NewSyncMap[int64, Tag]()

var livestreamMap = NewSyncMap[int64, LivestreamModel]()

var liveTagsMap = NewSyncListMap[int64, Tag]()

var userTotalReactionsMap = NewSyncCounterMap[int64, int64]()
var liveTotalReactionsMap = NewSyncCounterMap[int64, int64]()
var userTotalTipsMap = NewSyncCounterMap[int64, int64]()
var liveTotalTipsMap = NewSyncCounterMap[int64, int64]()

func LoadCache() {
	LoadIconFromDB()
	LoadUserReactionFromDB()
	LoadLiveReactionFromDB()
	LoadUserTipFromDB()
	LoadLivestreamFromDB()
	LoadThemeFromDB()
	LoadUserFromDB()
	LoadLivecommentsFromDB()
	LoadTagFromDB()
}

func LoadIconFromDB() {
	iconUserMap.Clear()
	iconUsernameMap.Clear()
	type IconWithUser struct {
		UserID   int64  `db:"user_id"`
		Image    []byte `db:"image"`
		Username string `db:"username"`
	}

	var rows []*IconWithUser
	if err := dbConn.Select(&rows, "SELECT icons.image as image, icons.user_id as user_id, users.name as username FROM icons INNER JOIN users ON icons.user_id = users.id"); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		hash := sha256.Sum256(row.Image)
		icon := Icon{Image: row.Image, Hash: hash}
		iconUserMap.Add(row.UserID, icon)
		iconUsernameMap.Add(row.Username, icon)
	}
}

func LoadUserReactionFromDB() {
	userTotalReactionsMap.Clear()

	type Reaction struct {
		UserID int64 `db:"user_id"`
		Count  int64 `db:"count"`
	}

	var rows []*Reaction
	if err := dbConn.Select(&rows, `SELECT COUNT(*) as count, u.id as user_id FROM users u
		INNER JOIN livestreams l ON l.user_id = u.id
		INNER JOIN reactions r ON r.livestream_id = l.id GROUP BY u.id`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		userTotalReactionsMap.Add(row.UserID, row.Count)
	}
}

func LoadLiveReactionFromDB() {
	liveTotalReactionsMap.Clear()

	type Reaction struct {
		LivestreamID int64 `db:"livestream_id"`
		Count        int64 `db:"count"`
	}

	var rows []*Reaction
	if err := dbConn.Select(&rows, `SELECT COUNT(*) as count, l.id as livestream_id FROM livestreams l INNER JOIN reactions r ON l.id = r.livestream_id GROUP BY l.id`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		liveTotalReactionsMap.Add(row.LivestreamID, row.Count)
	}
}

func LoadUserTipFromDB() {
	userTotalTipsMap.Clear()

	type Tip struct {
		UserID int64 `db:"user_id"`
		Count  int64 `db:"count"`
	}

	var rows []*Tip
	if err := dbConn.Select(&rows, `SELECT IFNULL(SUM(l2.tip), 0) as count, u.id as user_id FROM users u
		INNER JOIN livestreams l ON l.user_id = u.id	
		INNER JOIN livecomments l2 ON l2.livestream_id = l.id GROUP BY u.id`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		userTotalTipsMap.Add(row.UserID, row.Count)
	}
}

func LoadLiveTipsFromDB() {
	liveTotalTipsMap.Clear()

	type Tips struct {
		LivestreamID int64 `db:"livestream_id"`
		Count        int64 `db:"count"`
	}

	var rows []*Tips
	if err := dbConn.Select(&rows, `SELECT IFNULL(SUM(l2.tip), 0) as count, l.id as livestream_id FROM livestreams l INNER JOIN livecomments l2 ON l.id = l2.livestream_id GROUP BY l.id`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		liveTotalTipsMap.Add(row.LivestreamID, row.Count)
	}
}

func LoadLivestreamFromDB() {
	// clear sync map
	livestreamMap.Clear()

	var rows []*LivestreamModel
	if err := dbConn.Select(&rows, `SELECT * FROM livestreams`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		// add to sync map
		livestreamMap.Add(row.ID, *row)
	}
}

func LoadThemeFromDB() {
	// clear sync map
	themeMap.Clear()

	var rows []*ThemeModel
	if err := dbConn.Select(&rows, `SELECT * FROM themes`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		// add to sync map
		themeMap.Add(row.UserID, *row)
	}
}

func LoadUserFromDB() {
	// clear sync map
	userMap.Clear()

	var rows []*UserModel
	if err := dbConn.Select(&rows, `SELECT * FROM users`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		// add to sync map
		userMap.Add(row.ID, *row)
	}
}

func LoadLivecommentsFromDB() {
	// clear sync map
	liveTagsMap.Clear()

	type LivestreamTag struct {
		LivestreamID int64  `db:"livestream_id"`
		TagID        int64  `db:"tag_id"`
		TagName      string `db:"tag_name"`
	}

	var rows []*LivestreamTag
	if err := dbConn.Select(&rows, `SELECT l.livestream_id as livestream_id, l.tag_id as tag_id, t.name as tag_name FROM livestream_tags l LEFT JOIN tags t ON l.tag_id = t.id`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		// add to sync map
		liveTagsMap.Add(row.LivestreamID, Tag{ID: row.TagID, Name: row.TagName})
	}
}

func LoadTagFromDB() {
	// clear sync map
	tagMap.Clear()

	var rows []*TagModel
	if err := dbConn.Select(&rows, `SELECT * FROM tags`); err != nil {
		log.Fatalf("failed to load : %+v", err)
		return
	}
	for _, row := range rows {
		// add to sync map
		tagMap.Add(row.ID, Tag{ID: row.ID, Name: row.Name})
	}
}

func main() {
	http.DefaultServeMux.Handle("/debug/fgprof", fgprof.Handler())
	go func() {
		fmt.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	e := echo.New()
	e.Debug = true
	e.Logger.SetLevel(echolog.DEBUG)
	e.Use(middleware.Logger())
	cookieStore := sessions.NewCookieStore(secret)
	cookieStore.Options.Domain = "*.u.isucon.local"
	e.Use(session.Middleware(cookieStore))
	// e.Use(middleware.Recover())

	// 初期化
	e.POST("/api/initialize", initializeHandler)

	// top
	e.GET("/api/tag", getTagHandler)
	e.GET("/api/user/:username/theme", getStreamerThemeHandler)

	// livestream
	// reserve livestream
	e.POST("/api/livestream/reservation", reserveLivestreamHandler)
	// list livestream
	e.GET("/api/livestream/search", searchLivestreamsHandler)
	e.GET("/api/livestream", getMyLivestreamsHandler)
	e.GET("/api/user/:username/livestream", getUserLivestreamsHandler)
	// get livestream
	e.GET("/api/livestream/:livestream_id", getLivestreamHandler)
	// get polling livecomment timeline
	e.GET("/api/livestream/:livestream_id/livecomment", getLivecommentsHandler)
	// ライブコメント投稿
	e.POST("/api/livestream/:livestream_id/livecomment", postLivecommentHandler)
	e.POST("/api/livestream/:livestream_id/reaction", postReactionHandler)
	e.GET("/api/livestream/:livestream_id/reaction", getReactionsHandler)

	// (配信者向け)ライブコメントの報告一覧取得API
	e.GET("/api/livestream/:livestream_id/report", getLivecommentReportsHandler)
	e.GET("/api/livestream/:livestream_id/ngwords", getNgwords)
	// ライブコメント報告
	e.POST("/api/livestream/:livestream_id/livecomment/:livecomment_id/report", reportLivecommentHandler)
	// 配信者によるモデレーション (NGワード登録)
	e.POST("/api/livestream/:livestream_id/moderate", moderateHandler)

	// livestream_viewersにINSERTするため必要
	// ユーザ視聴開始 (viewer)
	e.POST("/api/livestream/:livestream_id/enter", enterLivestreamHandler)
	// ユーザ視聴終了 (viewer)
	e.DELETE("/api/livestream/:livestream_id/exit", exitLivestreamHandler)

	// user
	e.POST("/api/register", registerHandler)
	e.POST("/api/login", loginHandler)
	e.GET("/api/user/me", getMeHandler)
	// フロントエンドで、配信予約のコラボレーターを指定する際に必要
	e.GET("/api/user/:username", getUserHandler)
	e.GET("/api/user/:username/statistics", getUserStatisticsHandler)
	e.GET("/api/user/:username/icon", getIconHandler)
	e.POST("/api/icon", postIconHandler)

	// stats
	// ライブ配信統計情報
	e.GET("/api/livestream/:livestream_id/statistics", getLivestreamStatisticsHandler)

	// 課金情報
	e.GET("/api/payment", GetPaymentResult)

	e.HTTPErrorHandler = errorResponseHandler

	// DB接続
	conn, err := connectDB(e.Logger)
	if err != nil {
		e.Logger.Errorf("failed to connect db: %v", err)
		os.Exit(1)
	}
	defer conn.Close()
	dbConn = conn

	LoadCache()

	subdomainAddr, ok := os.LookupEnv(powerDNSSubdomainAddressEnvKey)
	if !ok {
		e.Logger.Errorf("environ %s must be provided", powerDNSSubdomainAddressEnvKey)
		os.Exit(1)
	}
	powerDNSSubdomainAddress = subdomainAddr

	// HTTPサーバ起動
	listenAddr := net.JoinHostPort("", strconv.Itoa(listenPort))
	if err := e.Start(listenAddr); err != nil {
		e.Logger.Errorf("failed to start HTTP server: %v", err)
		os.Exit(1)
	}
}

type ErrorResponse struct {
	Error string `json:"error"`
}

func errorResponseHandler(err error, c echo.Context) {
	c.Logger().Errorf("error at %s: %+v", c.Path(), err)
	if he, ok := err.(*echo.HTTPError); ok {
		if e := c.JSON(he.Code, &ErrorResponse{Error: err.Error()}); e != nil {
			c.Logger().Errorf("%+v", e)
		}
		return
	}

	if e := c.JSON(http.StatusInternalServerError, &ErrorResponse{Error: err.Error()}); e != nil {
		c.Logger().Errorf("%+v", e)
	}
}
