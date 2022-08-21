package pacseek

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"

	"github.com/Jguer/go-alpm/v2"
	pconf "github.com/Morganamilo/go-pacmanconf"
	"github.com/moson-mo/pacseek/internal/util"
)

// creates the alpm handler used to search packages
func initPacmanDbs(dbPath, confPath string, repos []string) (*alpm.Handle, error) {
	h, err := alpm.Initialize("/", dbPath)
	if err != nil {
		return nil, err
	}

	conf, _, err := pconf.ParseFile(confPath)
	if err != nil {
		return nil, err
	}

	for _, repo := range conf.Repos {
		if (len(repos) > 0 && util.SliceContains(repos, repo.Name)) || len(repos) == 0 {
			_, err := h.RegisterSyncDB(repo.Name, 0)
			if err != nil {
				return nil, err
			}
		}
	}
	return h, nil
}

// searches the pacman databases and returns packages that could be found (starting with "term")
func searchRepos(h *alpm.Handle, term string, mode string, by string, maxResults int, localOnly bool) ([]Package, error) {
	packages := []Package{}
	if h == nil {
		return packages, errors.New("alpm handle is nil")
	}
	dbs, err := h.SyncDBs()
	if err != nil {
		return packages, err
	}
	local, err := h.LocalDB()
	if err != nil {
		return packages, err
	}

	searchDbs := dbs.Slice()
	if localOnly {
		searchDbs = []alpm.IDB{local}
	}

	counter := 0
	for _, db := range searchDbs {
		for _, pkg := range db.PkgCache().Slice() {
			if counter >= maxResults {
				break
			}
			compFunc := strings.HasPrefix
			if mode == "Contains" {
				compFunc = strings.Contains
			}

			if compFunc(pkg.Name(), term) ||
				(by == "Name & Description" && compFunc(pkg.Description(), term)) {
				installed := false
				if local.Pkg(pkg.Name()) != nil {
					installed = true
				}
				packages = append(packages, Package{
					Name:         pkg.Name(),
					Source:       db.Name(),
					IsInstalled:  installed,
					LastModified: int(pkg.BuildDate().Unix()),
					Popularity:   math.MaxFloat64,
				})
				counter++
			}
		}
	}
	return packages, nil
}

// create/update temporary sync DB
func syncToTempDB(confPath string, repos []string) (*alpm.Handle, error) {
	// check if fakeroot is installed
	if _, err := os.Stat("/usr/bin/fakeroot"); errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("fakeroot not installed")
	}
	conf, _, err := pconf.ParseFile(confPath)
	if err != nil {
		return nil, err
	}
	/*
		We use the same naming as "checkupdates" to have less data to transfer
		in case the user already makes use of checkupdates...
	*/
	tmpdb := os.TempDir() + "/checkup-db-" + strconv.Itoa(os.Getuid())
	local := tmpdb + "/local"

	// create directory and symlink if needed
	if _, err := os.Stat(tmpdb); errors.Is(err, fs.ErrNotExist) {
		err := os.MkdirAll(tmpdb, 0755)
		if err != nil {
			return nil, err
		}
	}
	if _, err := os.Stat(local); errors.Is(err, fs.ErrNotExist) {
		err := os.Symlink(path.Join(conf.DBPath, "local"), local)
		if err != nil {
			return nil, err
		}
	}

	// execute pacman and sync to temporary db
	cmd := exec.Command("fakeroot", "--", "pacman", "-Sy", "--dbpath="+tmpdb)
	err = cmd.Run()
	if err != nil {
		return nil, err
	}

	h, err := initPacmanDbs(tmpdb, confPath, repos)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// returns packages that can be upgraded & packages that only exist locally
func getUpgradable(h *alpm.Handle) ([]Upgrade, []string) {
	upgradable := []Upgrade{}
	notFound := []string{}

	if h == nil {
		return upgradable, notFound
	}
	dbs, err := h.SyncDBs()
	if err != nil {
		return upgradable, notFound
	}
	local, err := h.LocalDB()
	if err != nil {
		return upgradable, notFound
	}

	for _, lpkg := range local.PkgCache().Slice() {
		found := false
		for _, db := range dbs.Slice() {
			pkg := db.Pkg(lpkg.Name())
			if pkg != nil {
				found = true
				if alpm.VerCmp(pkg.Version(), lpkg.Version()) > 0 {
					upgradable = append(upgradable, Upgrade{
						Name:         pkg.Name(),
						Version:      pkg.Version(),
						LocalVersion: lpkg.Version(),
						Source:       db.Name(),
					})
				}
				break
			}
		}
		if !found {
			upgradable = append(upgradable, Upgrade{
				Name:         lpkg.Name(),
				LocalVersion: lpkg.Version(),
				Source:       "local",
			})
			notFound = append(notFound, lpkg.Name())
		}
	}
	return upgradable, notFound
}

// checks the local db if a package is installed
func isPackageInstalled(h *alpm.Handle, pkg string) bool {
	local, err := h.LocalDB()
	if err != nil {
		return false
	}
	local.SetUsage(alpm.UsageSearch)

	return local.Pkg(pkg) != nil
}

// retrieves package information from the pacman DB's and returns it in the same format as the AUR call
func infoPacman(h *alpm.Handle, pkgs []string) RpcResult {
	r := RpcResult{
		Results: []InfoRecord{},
	}

	dbs, err := h.SyncDBs()
	if err != nil {
		r.Error = err.Error()
		return r
	}

	local, err := h.LocalDB()
	if err != nil {
		r.Error = err.Error()
		return r
	}
	dbslice := append(dbs.Slice(), local)

	for _, pkg := range pkgs {
		for _, db := range dbslice {
			p := db.Pkg(pkg)
			if p == nil {
				continue
			}

			deps := []string{}
			makedeps := []string{}
			odeps := []string{}
			prov := []string{}
			conf := []string{}
			for _, d := range p.Depends().Slice() {
				deps = append(deps, d.Name)
			}
			for _, d := range p.MakeDepends().Slice() {
				makedeps = append(makedeps, d.Name)
			}
			for _, d := range p.OptionalDepends().Slice() {
				odeps = append(odeps, d.Name)
			}

			i := InfoRecord{
				Name:         p.Name(),
				Description:  p.Description(),
				Provides:     prov,
				Conflicts:    conf,
				Version:      p.Version(),
				License:      p.Licenses().Slice(),
				Maintainer:   p.Packager(),
				Depends:      deps,
				MakeDepends:  makedeps,
				OptDepends:   odeps,
				URL:          p.URL(),
				LastModified: int(p.BuildDate().UTC().Unix()),
				Source:       db.Name(),
				Architecture: p.Architecture(),
				RequiredBy:   p.ComputeRequiredBy(),
				PackageBase:  p.Base(),
			}
			if db.Name() == "local" {
				i.Description = p.Description() + "\n[red]* Package not found in repositories/AUR *"
			}
			r.Results = append(r.Results, i)
			break
		}
	}
	return r
}
