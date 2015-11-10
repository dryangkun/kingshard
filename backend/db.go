package backend

import (
	"sync"
	"sync/atomic"

	"github.com/flike/kingshard/core/errors"
	"github.com/flike/kingshard/mysql"
)

const (
	Up = iota
	Down
	Unknown

	InitConnCount     = 16
	DefaultMaxConnNum = 10000
	DefaultWait       = 4
)

type DB struct {
	sync.Mutex

	addr     string
	user     string
	password string
	db       string
	state    int32

	maxConnNum  int
	InitConnNum int
	connNum     int32
	idleConns   chan *Conn
	checkConn   *Conn
}

func Open(addr string, user string, password string, dbName string, maxConnNum int) (*DB, error) {
	db := new(DB)
	db.addr = addr
	db.user = user
	db.password = password
	db.db = dbName
	if 0 < maxConnNum {
		db.maxConnNum = maxConnNum
		if db.maxConnNum < 16 {
			db.InitConnNum = db.maxConnNum
		} else {
			db.InitConnNum = db.maxConnNum / 4
		}

	} else {
		db.maxConnNum = DefaultMaxConnNum
		db.InitConnNum = InitConnCount
	}

	db.idleConns = make(chan *Conn, db.maxConnNum)
	db.connNum = 0
	atomic.StoreInt32(&(db.state), Unknown)

	for i := 0; i < db.InitConnNum; i++ {
		atomic.AddInt32(&(db.connNum), 1)
		conn, err := db.newConn()
		if err != nil {
			db.Close()
			return nil, errors.ErrDBPoolInit
		}
		db.idleConns <- conn
	}
	db.checkConn = <-db.idleConns

	return db, nil
}

func (db *DB) Addr() string {
	return db.addr
}

func (db *DB) State() string {
	var state string
	switch db.state {
	case Up:
		state = "up"
	case Down:
		state = "down"
	case Unknown:
		state = "unknow"
	}
	return state
}

func (db *DB) IdleConnCount() int {
	db.Lock()
	defer db.Unlock()
	return len(db.idleConns)
}

func (db *DB) Close() error {
	db.Lock()
	connChannel := db.idleConns
	db.idleConns = nil
	db.connNum = 0
	db.Unlock()
	if connChannel == nil {
		return nil
	}
	close(connChannel)
	for conn := range connChannel {
		db.closeConn(conn)
	}

	return nil
}

func (db *DB) getConns() chan *Conn {
	db.Lock()
	conns := db.idleConns
	db.Unlock()
	return conns
}

func (db *DB) Ping() error {
	var err error
	if db.checkConn == nil {
		db.checkConn, err = db.newConn()
		if err != nil {
			return err
		}
	}
	err = db.checkConn.Ping()
	return err
}

func (db *DB) newConn() (*Conn, error) {
	co := new(Conn)

	if err := co.Connect(db.addr, db.user, db.password, db.db); err != nil {
		return nil, err
	}

	return co, nil
}

func (db *DB) closeConn(co *Conn) error {
	if co != nil {
		atomic.AddInt32(&(db.connNum), -1)
		return co.Close()
	} else {
		return nil
	}
}

func (db *DB) tryReuse(co *Conn) error {
	if co.IsInTransaction() {
		//we can not reuse a connection in transaction status
		if err := co.Rollback(); err != nil {
			return err
		}
	}

	if !co.IsAutoCommit() {
		//we can not  reuse a connection not in autocomit
		if _, err := co.exec("set autocommit = 1"); err != nil {
			return err
		}
	}

	//connection may be set names early
	//we must use default utf8
	if co.GetCharset() != mysql.DEFAULT_CHARSET {
		if err := co.SetCharset(mysql.DEFAULT_CHARSET); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) PopConn() (*Conn, error) {
	var co *Conn
	var err error

	conns := db.getConns()
	if conns == nil {
		return nil, errors.ErrDatabaseClose
	}
	if 0 < len(conns) {
		co = <-conns
	} else {
		db.Lock()
		if int(db.connNum) < db.maxConnNum {
			db.connNum++
			db.Unlock()
			co, err = db.newConn()
			if err != nil {
				db.closeConn(co)
				return nil, err
			}
			return co, nil
		} else {
			db.Unlock()
			co = <-conns
		}
	}

	if co == nil {
		return nil, errors.ErrConnIsNil
	}
	if err = co.Ping(); err == nil {
		if err = db.tryReuse(co); err == nil {
			return co, nil
		}
	}
	db.closeConn(co)
	return nil, errors.ErrPopConnFail
}

func (db *DB) PushConn(co *Conn, err error) {
	db.Lock()
	defer db.Unlock()
	if co == nil {
		return
	}
	if err != nil || db.idleConns == nil {
		db.closeConn(co)
		return
	}

	select {
	case db.idleConns <- co:
		return
	default:
		db.closeConn(co)
		return
	}
}

type BackendConn struct {
	*Conn

	db *DB
}

func (p *BackendConn) Close() {
	if p.Conn != nil {
		if p.Conn.pkgErr != nil {
			p.db.closeConn(p.Conn)
		} else {
			p.db.PushConn(p.Conn, p.Conn.pkgErr)
			p.Conn = nil
		}
	}
}

func (db *DB) GetConn() (*BackendConn, error) {
	c, err := db.PopConn()
	if err != nil {
		return nil, err
	}
	return &BackendConn{c, db}, nil
}
