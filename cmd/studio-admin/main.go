// studio-admin bootstraps an administrator without a public privilege endpoint.
package main

import (
	"av-platform/business/internal/infrastructure/database"
	"av-platform/business/internal/platform/identity"
	"context"
	"flag"
	"log"
	"os"
)

func main() {
	dbpath := flag.String("db", "remote-control.sqlite", "Database file")
	name := flag.String("name", "admin", "Administrator username")
	flag.Parse()
	db, e := database.OpenSQLite(*dbpath)
	if e != nil {
		log.Fatal(e)
	}
	defer db.Close()
	s, e := identity.New(db)
	if e != nil {
		log.Fatal(e)
	}
	u, e := s.Register(context.Background(), *name, os.Getenv("STUDIO_ADMIN_PASSWORD"), true)
	if e != nil {
		log.Fatal(e)
	}
	log.Printf("Created administrator %s", u.Name)
}
