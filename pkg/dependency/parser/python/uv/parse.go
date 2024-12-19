package uv

import (
	"sort"

	"github.com/BurntSushi/toml"
	"github.com/samber/lo"
	"golang.org/x/xerrors"

	"github.com/aquasecurity/trivy/pkg/dependency"
	ftypes "github.com/aquasecurity/trivy/pkg/fanal/types"
	xio "github.com/aquasecurity/trivy/pkg/x/io"
)

type Lock struct {
	Packages []Package `toml:"package"`
}

func (l Lock) packages() map[string]Package {
	return lo.SliceToMap(l.Packages, func(pkg Package) (string, Package) {
		return pkg.Name, pkg
	})
}

func (l Lock) directDeps(root Package) map[string]struct{} {
	deps := make(map[string]struct{})
	for _, dep := range root.Dependencies {
		deps[dep.Name] = struct{}{}
	}
	return deps
}

func prodDeps(root Package, packages map[string]Package) map[string]struct{} {
	visited := make(map[string]struct{})
	walkPackageDeps(root, packages, visited)
	return visited
}

func walkPackageDeps(pkg Package, packages map[string]Package, visited map[string]struct{}) {
	if _, ok := visited[pkg.Name]; ok {
		return
	}
	visited[pkg.Name] = struct{}{}
	for _, dep := range pkg.Dependencies {
		depPkg, exists := packages[dep.Name]
		if !exists {
			continue
		}
		walkPackageDeps(depPkg, packages, visited)
	}
}

func (l Lock) root() (Package, error) {
	var pkgs []Package
	for _, pkg := range l.Packages {
		if pkg.isRoot() {
			pkgs = append(pkgs, pkg)
		}
	}

	// lock file must include root package
	// cf. https://github.com/astral-sh/uv/blob/f80ddf10b63c3e7b421ca4658e63f97db1e0378c/crates/uv/src/commands/project/lock.rs#L933-L936
	if len(pkgs) != 1 {
		return Package{}, xerrors.New("uv lockfile must contain 1 root package")
	}

	return pkgs[0], nil
}

type Package struct {
	Name         string       `toml:"name"`
	Version      string       `toml:"version"`
	Source       Source       `toml:"source"`
	Dependencies []Dependency `toml:"dependencies"`
}

// https://github.com/astral-sh/uv/blob/f7d647e81d7e1e3be189324b06024ed2057168e6/crates/uv-resolver/src/lock/mod.rs#L572-L579
func (p Package) isRoot() bool {
	return p.Source.Editable == "." || p.Source.Virtual == "."
}

type Source struct {
	Editable string `toml:"editable"`
	Virtual  string `toml:"virtual"`
}

type Dependency struct {
	Name string `toml:"name"`
}

type Parser struct{}

func NewParser() *Parser {
	return &Parser{}
}

func (p *Parser) Parse(r xio.ReadSeekerAt) ([]ftypes.Package, []ftypes.Dependency, error) {
	var lock Lock
	if _, err := toml.NewDecoder(r).Decode(&lock); err != nil {
		return nil, nil, xerrors.Errorf("failed to decode uv lock file: %w", err)
	}

	rootPackage, err := lock.root()
	if err != nil {
		return nil, nil, err
	}

	packages := lock.packages()
	directDeps := lock.directDeps(rootPackage)

	// Since each lockfile contains a root package with a list of direct dependencies,
	// we can identify all production dependencies by traversing the dependency graph
	// and collecting all the dependencies that are reachable from the root.
	prodDeps := prodDeps(rootPackage, packages)

	var (
		pkgs ftypes.Packages
		deps ftypes.Dependencies
	)

	for _, pkg := range lock.Packages {
		if _, ok := prodDeps[pkg.Name]; !ok {
			continue
		}

		pkgID := packageID(pkg.Name, pkg.Version)
		relationship := ftypes.RelationshipIndirect
		if pkg.isRoot() {
			relationship = ftypes.RelationshipRoot
		} else if _, ok := directDeps[pkg.Name]; ok {
			relationship = ftypes.RelationshipDirect
		}

		pkgs = append(pkgs, ftypes.Package{
			ID:           pkgID,
			Name:         pkg.Name,
			Version:      pkg.Version,
			Relationship: relationship,
		})

		dependsOn := make([]string, 0, len(pkg.Dependencies))

		for _, dep := range pkg.Dependencies {
			depPkg, exists := packages[dep.Name]
			if !exists {
				continue
			}
			dependsOn = append(dependsOn, packageID(dep.Name, depPkg.Version))
		}

		if len(dependsOn) > 0 {
			sort.Strings(dependsOn)
			deps = append(deps, ftypes.Dependency{
				ID:        pkgID,
				DependsOn: dependsOn,
			})
		}
	}

	sort.Sort(pkgs)
	sort.Sort(deps)
	return pkgs, deps, nil
}

func packageID(name, version string) string {
	return dependency.ID(ftypes.Uv, name, version)
}
