package graphdb

import (
	"database/sql"
	"fmt"
	"path"
	"strings"
	"sync"

	log "github.com/Sirupsen/logrus"
)

const (
	createEntityTable = `
    CREATE TABLE IF NOT EXISTS entity (
        id text NOT NULL PRIMARY KEY
    );`

	createEdgeTable = `
    CREATE TABLE IF NOT EXISTS edge (
        "entity_id" text NOT NULL,
        "parent_id" text NULL,
        "name" text NOT NULL,
        CONSTRAINT "parent_fk" FOREIGN KEY ("parent_id") REFERENCES "entity" ("id"),
        CONSTRAINT "entity_fk" FOREIGN KEY ("entity_id") REFERENCES "entity" ("id")
        );
    `

	createEdgeIndices = `
    CREATE UNIQUE INDEX IF NOT EXISTS "name_parent_ix" ON "edge" (parent_id, name);
    `
)

// Entity with a unique id
type Entity struct {
	id string
}

// An Edge connects two entities together
type Edge struct {
	EntityID string
	Name     string
	ParentID string
}

type Entities map[string]*Entity
type Edges []*Edge

type WalkFunc func(fullPath string, entity *Entity) error

// Graph database for storing entities and their relationships
type Database struct {
	conn *sql.DB
	mux  sync.RWMutex
}

func IsNonUniqueNameError(err error) bool {
	str := err.Error()
	// sqlite 3.7.17-1ubuntu1 returns:
	// Set failure: Abort due to constraint violation: columns parent_id, name are not unique
	if strings.HasSuffix(str, "name are not unique") {
		return true
	}
	// sqlite-3.8.3-1.fc20 returns:
	// Set failure: Abort due to constraint violation: UNIQUE constraint failed: edge.parent_id, edge.name
	if strings.Contains(str, "UNIQUE constraint failed") && strings.Contains(str, "edge.name") {
		return true
	}
	// sqlite-3.6.20-1.el6 returns:
	// Set failure: Abort due to constraint violation: constraint failed
	if strings.HasSuffix(str, "constraint failed") {
		return true
	}
	return false
}

// Create a new graph database initialized with a root entity
func NewDatabase(conn *sql.DB) (*Database, error) {
	if conn == nil {
		return nil, fmt.Errorf("Database connection cannot be nil")
	}
	db := &Database{conn: conn}

	if _, err := conn.Exec(createEntityTable); err != nil {
		return nil, err
	}
	if _, err := conn.Exec(createEdgeTable); err != nil {
		return nil, err
	}
	if _, err := conn.Exec(createEdgeIndices); err != nil {
		return nil, err
	}

	deleteEdgeQuery, err := db.conn.Prepare("DELETE FROM edge where entity_id=? and name=?")
	if err != nil {
		return nil, err
	}
	deleteEntityQuery, err := db.conn.Prepare("DELETE FROM entity where id = ?;")
	if err != nil {
		return nil, err
	}
	insertEntityQuery, err := db.conn.Prepare("INSERT INTO entity (id) VALUES (?);")
	if err != nil {
		return nil, err
	}
	insertEdgeQuery, err := db.conn.Prepare("INSERT INTO edge (entity_id, name) VALUES(?,?);")
	if err != nil {
		return nil, err
	}

	// Create root entities
	tx, err := conn.Begin()
	if err != nil {
		return nil, err
	}

	if _, err := tx.Stmt(deleteEntityQuery).Exec("0"); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return nil, err
	}

	if _, err := tx.Stmt(insertEntityQuery).Exec("0"); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return nil, err
	}

	if _, err := tx.Stmt(deleteEdgeQuery).Exec("0", "/"); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return nil, err
	}

	if _, err := tx.Stmt(insertEdgeQuery).Exec("0", "/"); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return db, nil
}

// Close the underlying connection to the database
func (db *Database) Close() error {
	return db.conn.Close()
}

// Set the entity id for a given path
func (db *Database) Set(fullPath, id string) (*Entity, error) {
	db.mux.Lock()
	defer db.mux.Unlock()

	selectEntityQuery, err := db.conn.Prepare("SELECT id FROM entity WHERE id = ?;")
	if err != nil {
		return nil, err
	}
	insertEntityQuery, err := db.conn.Prepare("INSERT INTO entity (id) VALUES(?);")
	if err != nil {
		return nil, err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return nil, err
	}
	var entityID string
	if err := tx.Stmt(selectEntityQuery).QueryRow(id).Scan(&entityID); err != nil {
		if err == sql.ErrNoRows {
			if _, err := tx.Stmt(insertEntityQuery).Exec(id); err != nil {
				if err := tx.Rollback(); err != nil {
					log.Warnf("graphdb rollback failed: %s", err)
				}
				return nil, err
			}
		} else {
			if err := tx.Rollback(); err != nil {
				log.Warnf("graphdb rollback failed: %s", err)
			}
			return nil, err
		}
	}
	e := &Entity{id}

	parentPath, name := splitPath(fullPath)
	if err := db.setEdge(tx, parentPath, name, e); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return e, nil
}

// Return true if a name already exists in the database
func (db *Database) Exists(name string) bool {
	db.mux.RLock()
	defer db.mux.RUnlock()

	e, err := db.get(name)
	if err != nil {
		return false
	}
	return e != nil
}

func (db *Database) setEdge(tx *sql.Tx, parentPath, name string, e *Entity) error {
	parent, err := db.get(parentPath)
	if err != nil {
		return err
	}
	if parent.id == e.id {
		return fmt.Errorf("Cannot set self as child")
	}

	if _, err := tx.Exec("INSERT INTO edge (parent_id, name, entity_id) VALUES (?,?,?);", parent.id, name, e.id); err != nil {
		return err
	}
	return nil
}

// Return the root "/" entity for the database
func (db *Database) RootEntity() *Entity {
	return &Entity{
		id: "0",
	}
}

// Return the entity for a given path
func (db *Database) Get(name string) *Entity {
	db.mux.RLock()
	defer db.mux.RUnlock()

	e, err := db.get(name)
	if err != nil {
		return nil
	}
	return e
}

func (db *Database) get(name string) (*Entity, error) {
	e := db.RootEntity()
	// We always know the root name so return it if
	// it is requested
	if name == "/" {
		return e, nil
	}

	parts := split(name)
	for i := 1; i < len(parts); i++ {
		p := parts[i]
		if p == "" {
			continue
		}

		next := db.child(e, p)
		if next == nil {
			return nil, fmt.Errorf("Cannot find child for %s", name)
		}
		e = next
	}
	return e, nil

}

// List all entities by from the name
// The key will be the full path of the entity
func (db *Database) List(name string, depth int) Entities {
	db.mux.RLock()
	defer db.mux.RUnlock()

	out := Entities{}
	e, err := db.get(name)
	if err != nil {
		return out
	}

	children, err := db.children(e, name, depth, nil)
	if err != nil {
		return out
	}

	for _, c := range children {
		out[c.FullPath] = c.Entity
	}
	return out
}

// Walk through the child graph of an entity, calling walkFunc for each child entity.
// It is safe for walkFunc to call graph functions.
func (db *Database) Walk(name string, walkFunc WalkFunc, depth int) error {
	children, err := db.Children(name, depth)
	if err != nil {
		return err
	}

	// Note: the database lock must not be held while calling walkFunc
	for _, c := range children {
		if err := walkFunc(c.FullPath, c.Entity); err != nil {
			return err
		}
	}
	return nil
}

// Return the children of the specified entity
func (db *Database) Children(name string, depth int) ([]WalkMeta, error) {
	db.mux.RLock()
	defer db.mux.RUnlock()

	e, err := db.get(name)
	if err != nil {
		return nil, err
	}

	return db.children(e, name, depth, nil)
}

// Return the parents of a specified entity
func (db *Database) Parents(name string) ([]string, error) {
	db.mux.RLock()
	defer db.mux.RUnlock()

	e, err := db.get(name)
	if err != nil {
		return nil, err
	}
	return db.parents(e)
}

// Return the refrence count for a specified id
func (db *Database) Refs(id string) int {
	db.mux.RLock()
	defer db.mux.RUnlock()

	var count int
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM edge WHERE entity_id = ?;", id).Scan(&count); err != nil {
		return 0
	}
	return count
}

// Return all the id's path references
func (db *Database) RefPaths(id string) Edges {
	db.mux.RLock()
	defer db.mux.RUnlock()

	refs := Edges{}

	rows, err := db.conn.Query("SELECT name, parent_id FROM edge WHERE entity_id = ?;", id)
	if err != nil {
		return refs
	}
	defer rows.Close()

	for rows.Next() {
		var name string
		var parentID string
		if err := rows.Scan(&name, &parentID); err != nil {
			return refs
		}
		refs = append(refs, &Edge{
			EntityID: id,
			Name:     name,
			ParentID: parentID,
		})
	}
	return refs
}

// Delete the reference to an entity at a given path
func (db *Database) Delete(name string) error {
	db.mux.Lock()
	defer db.mux.Unlock()

	if name == "/" {
		return fmt.Errorf("Cannot delete root entity")
	}

	parentPath, n := splitPath(name)
	parent, err := db.get(parentPath)
	if err != nil {
		return err
	}

	if _, err := db.conn.Exec("DELETE FROM edge WHERE parent_id = ? AND name = ?;", parent.id, n); err != nil {
		return err
	}
	return nil
}

// Remove the entity with the specified id
// Walk the graph to make sure all references to the entity
// are removed and return the number of references removed
func (db *Database) Purge(id string) (int, error) {
	db.mux.Lock()
	defer db.mux.Unlock()

	deleteEdgeQuery, err := db.conn.Prepare("DELETE FROM edge WHERE entity_id = ?;")
	if err != nil {
		return -1, err
	}
	deleteEntityQuery, err := db.conn.Prepare("DELETE FROM entity where id = ?;")
	if err != nil {
		return -1, err
	}

	tx, err := db.conn.Begin()
	if err != nil {
		return -1, err
	}

	// Delete all edges
	rows, err := tx.Stmt(deleteEdgeQuery).Exec(id)
	if err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return -1, err
	}

	changes, err := rows.RowsAffected()
	if err != nil {
		return -1, err
	}

	// Delete entity
	if _, err := tx.Stmt(deleteEntityQuery).Exec(id); err != nil {
		if err := tx.Rollback(); err != nil {
			log.Warnf("graphdb rollback failed: %s", err)
		}
		return -1, err
	}

	if err := tx.Commit(); err != nil {
		return -1, err
	}
	return int(changes), nil
}

// Rename an edge for a given path
func (db *Database) Rename(currentName, newName string) error {
	db.mux.Lock()
	defer db.mux.Unlock()

	parentPath, name := splitPath(currentName)
	newParentPath, newEdgeName := splitPath(newName)

	if parentPath != newParentPath {
		return fmt.Errorf("Cannot rename when root paths do not match %s != %s", parentPath, newParentPath)
	}

	parent, err := db.get(parentPath)
	if err != nil {
		return err
	}

	rows, err := db.conn.Exec("UPDATE edge SET name = ? WHERE parent_id = ? AND name = ?;", newEdgeName, parent.id, name)
	if err != nil {
		return err
	}
	i, err := rows.RowsAffected()
	if err != nil {
		return err
	}
	if i == 0 {
		return fmt.Errorf("Cannot locate edge for %s %s", parent.id, name)
	}
	return nil
}

type WalkMeta struct {
	Parent   *Entity
	Entity   *Entity
	FullPath string
	Edge     *Edge
}

func (db *Database) children(e *Entity, name string, depth int, entities []WalkMeta) ([]WalkMeta, error) {
	if e == nil {
		return entities, nil
	}

	rows, err := db.conn.Query("SELECT entity_id, name FROM edge where parent_id = ?;", e.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var entityID, entityName string
		if err := rows.Scan(&entityID, &entityName); err != nil {
			return nil, err
		}
		child := &Entity{entityID}
		edge := &Edge{
			ParentID: e.id,
			Name:     entityName,
			EntityID: child.id,
		}

		meta := WalkMeta{
			Parent:   e,
			Entity:   child,
			FullPath: path.Join(name, edge.Name),
			Edge:     edge,
		}

		entities = append(entities, meta)

		if depth != 0 {
			nDepth := depth
			if depth != -1 {
				nDepth -= 1
			}
			entities, err = db.children(child, meta.FullPath, nDepth, entities)
			if err != nil {
				return nil, err
			}
		}
	}

	return entities, nil
}

func (db *Database) parents(e *Entity) (parents []string, err error) {
	if e == nil {
		return parents, nil
	}

	rows, err := db.conn.Query("SELECT parent_id FROM edge where entity_id = ?;", e.id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var parentID string
		if err := rows.Scan(&parentID); err != nil {
			return nil, err
		}
		parents = append(parents, parentID)
	}

	return parents, nil
}

// Return the entity based on the parent path and name
func (db *Database) child(parent *Entity, name string) *Entity {
	var id string
	if err := db.conn.QueryRow("SELECT entity_id FROM edge WHERE parent_id = ? AND name = ?;", parent.id, name).Scan(&id); err != nil {
		return nil
	}
	return &Entity{id}
}

// Return the id used to reference this entity
func (e *Entity) ID() string {
	return e.id
}

// Return the paths sorted by depth
func (e Entities) Paths() []string {
	out := make([]string, len(e))
	var i int
	for k := range e {
		out[i] = k
		i++
	}
	sortByDepth(out)

	return out
}
