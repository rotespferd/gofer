// Package gofer implements a task manager.
package gofer

import (
  "errors"
  "fmt"
  "go/ast"
  "go/parser"
  "go/token"
  "io"
  "os"
  "os/exec"
  "path"
  "path/filepath"
  "runtime"
  "strings"
  "text/template"
  "time"
)

const (
  DELIMITER            = ":"
  SOURCE_PREFIX        = "/src/"
  PACKAGE_NAME         = "tasks"
  EXPECTED_IMPORT      = "gofer"
  TEMPLATE_DESTINATION = "gofer_task_definitions_%v.go"
)

var (
  ErrBadLabel                 = errors.New("Bad label for task, unexpected section delimiter.")
  ErrRegistrationFailure      = errors.New("Registration for task failed unexpectedly.")
  ErrUnknownTask              = errors.New("Unable to look up task.")
  ErrNoAction                 = errors.New("No action defined for task.")
  ErrUnsetGoPath              = errors.New("Environment variable for GOPATH could not be found.")
  ErrUnresolvableDependencies = errors.New("Unable to resolve dependencies.")
  ErrCyclicDependency         = errors.New("Cyclic dependency detected.")
)

type action func(...string) error

type Task struct {
  Namespace    string       // Namespace or namespace the task is to live under.
  Label        string       // Label or name of the task.
  Description  string       // Description of what the task does.
  Dependencies dependencies // Dependencies of the task, or definitions of other tasks to preform
  Action       action       // Action function to run when task is executed.
  manual       manual       // Subtasks
  location     string       // Package location the task was registered from.
}

type manual []*Task
type dependencies []string

type imprt struct {
  Path string
}

var templateData struct {
  Imports []struct {
    Path string
  }
}

var (
  gofer       = make(manual, 0)     // gofer variable used for storing tasks.
  directories = make([]string, 0)   // potential task directories.
  goPath      = os.Getenv("GOPATH") // local GOPATH environment variable.
)

var loader = template.Must(template.New("loader").Parse(`
  // This file was generated by gofer (www.github.com/chuckpreslar/gofer)
  // copyright (c) Chuck Preslar, 2013
  package main

  // Imported standard lib packages.
  import (
    "os"
  )
  
  // Import the gofer package.
  import(
    "github.com/chuckpreslar/gofer"
  )
  
  // Imported task packages.
  import(
  {{range .Imports}}
    _ "{{.Path}}"
  {{end}}
  )

  func main() {
    // Template is executed to register and preform tasks.
    gofer.Perform(os.Args[1], os.Args[2:]...)
  }
`))

func printFailureNotice(err error) {
  fmt.Fprintf(os.Stderr, "[ \033[31m\u2717\033[0m ] %v\n", err)
}

func printSuccessNotice(msg string) {
  fmt.Fprintf(os.Stderr, "[ \033[32m\u2713\033[0m ] %v\n", msg)
}

// includes is a helper to reduce duplicated code, checking
// to see if `dependencies` string slice contains the provided
// definition.
func (self dependencies) includes(definition string) bool {
  for _, dependency := range self {
    if dependency == definition {
      return true
    }
  }

  return false
}

// add appends to the `dependencies` slice the provided `definition`.
func (self *dependencies) add(definition string) {
  *self = append(*self, definition)
}

// remove removes the `definition` from the `dependencies` slice if
// it's found.
func (self *dependencies) remove(definition string) {
  for index, dependency := range *self {
    if dependency == definition {
      (*self) = append((*self)[:index], (*self)[index+1:]...)
    }
  }
}

// index searches through the manual, returning a task
// found with the label and in the section (namepsace) defined
// by the definition.
func (self manual) index(definition string) (task *Task) {
  sections := strings.Split(definition, DELIMITER)
  entries := self

  for _, section := range sections {
    for i := 0; i < len(entries); i++ {
      if entries[i].Label == section {
        task = entries[i]
        entries = task.manual // adjust `entries` pointer for next iteration.
        break
      }
    }

    if nil == task {
      return
    } else if section != task.Label {
      return nil
    }
  }

  return
}

// sectionalize creates potential missing spaces in a manual
// based on the `definition`.
func (self *manual) sectionalize(definition string) (task *Task) {
  task = self.index(definition)

  if nil != task {
    return
  }

  sections := strings.Split(definition, DELIMITER)

  task = new(Task)
  task.Label = sections[0]

  *self = append(*self, task)

  for i := 1; i < len(sections); i++ {
    temp := new(Task)
    temp.Namespace = strings.Join(sections[:i], DELIMITER)
    temp.Label = sections[i]

    task.manual = append(task.manual, temp)
    task = temp // update task to temp for next iteration.
  }

  return
}

// rewrite copys values for Actions, Dependencies, and Description
// from one task to another.
func (self *Task) rewrite(task Task) {
  self.Description = task.Description
  self.Action = task.Action

  if 0 == len(self.location) || self.location == task.location {
    self.Dependencies = append(self.Dependencies, task.Dependencies...)
  } else {
    self.Dependencies = task.Dependencies
  }
}

// Register accepts a `Task`, storing it for later.
func Register(task Task) (err error) {
  if index := strings.Index(task.Label, DELIMITER); -1 != index {
    printFailureNotice(ErrBadLabel)
    return ErrBadLabel
  }

  _, task.location, _, _ = runtime.Caller(1)

  if defined := gofer.index(strings.Join([]string{task.Namespace, task.Label}, DELIMITER)); nil != defined {
    // FIXME: This action should be logged if defined.location !~ task.location.
    defined.rewrite(task)

    return
  }

  parent := gofer.sectionalize(task.Namespace)

  if nil == parent {
    if 0 != len(task.Namespace) {
      return ErrRegistrationFailure
    }

    gofer = append(gofer, &task)
  } else {
    parent.manual = append(parent.manual, &task)
  }

  return
}

// LoadAndPerform attempts to load tasks by executing
// a generated main package and preforming a Task's Action based
// on the definition.
func LoadAndPerform(definition string, arguments ...string) (err error) {
  err = load(definition, arguments...)

  if nil != err {
    printFailureNotice(err)
  }

  return err
}

// Perform attempts to preform a Task already loaded.
func Perform(definition string, arguments ...string) (err error) {
  if nil == gofer.index(definition) {
    printFailureNotice(ErrUnknownTask)
    return ErrUnknownTask
  }

  definitions, err := calculateDependencies(definition)

  if nil != err {
    printFailureNotice(err)
    return
  }

  for _, definition = range definitions {
    task := gofer.index(definition)

    if nil != task.Action {
      if err = task.Action(arguments...); nil != err {
        // Failed to execute task or dependency.
        printFailureNotice(errors.New(fmt.Sprintf("Task %v failed to executed", definition)))
        printFailureNotice(errors.New(fmt.Sprintf("Error: %v", err)))
        return
      } else {
        printSuccessNotice(fmt.Sprintf("Successfully preformed task %v", definition))
        // Executes successfully.
      }
    }
  }

  return
}

// load attempts to load all potential gofer tasks
// found within the local GOPATH environment variable.
func load(definition string, arguments ...string) (err error) {
  if err = walk(); nil != err {
    return
  }

  if err = parse(); nil != err {
    return
  }

  dir := path.Join(os.TempDir(), fmt.Sprintf(TEMPLATE_DESTINATION, time.Now().Unix()))

  if err = write(dir); nil != err {
    return
  }

  defer func() {
    err = remove(dir)
  }()

  command := exec.Command("go", append([]string{"run", dir, definition}, arguments...)...)
  stdout, err := command.StdoutPipe()

  if nil != err {
    return err
  }

  stderr, err := command.StderrPipe()

  if nil != err {
    return err
  }

  if err = command.Start(); nil != err {
    return
  }

  go io.Copy(os.Stdout, stdout)
  go io.Copy(os.Stderr, stderr)

  if err = command.Wait(); nil != err {
    return
  }

  return
}

// walk walks the local GOPATH environment variable, looking for
// directories with the `PACKAGE_NAME` of "tasks"
func walk() (err error) {
  if 0 == len(goPath) {
    return ErrUnsetGoPath
  }

  err = filepath.Walk(goPath, func(path string, info os.FileInfo, err error) error {
    if info.IsDir() && strings.HasSuffix(path, PACKAGE_NAME) {
      directories = append(directories, path)
    }

    return err
  })

  return
}

// parse attempts to load each "tasks" directory found within
// the local GOPATH environment variable into the Go parser.
func parse() (err error) {
  for _, dir := range directories {
    fset := token.NewFileSet()
    packages, err := parser.ParseDir(fset, dir, nil, parser.AllErrors)

    if nil != err {
      return err
    }

    parsePackages(packages, dir)
  }

  return nil
}

// parsePackages inspects Go AST packages to ensure the files
// are intended to register Tasks with or use the gofer package.
func parsePackages(packages map[string]*ast.Package, dir string) {
  for _, pkg := range packages {
    file := ast.MergePackageFiles(pkg, ast.FilterImportDuplicates)

    if isGoferTaskFile(file) {
      imprtPath := strings.TrimLeft(strings.Replace(dir, goPath, "", 1), SOURCE_PREFIX)
      templateData.Imports = append(templateData.Imports, imprt{imprtPath})
    }
  }
}

// write attempts to write the `loader` template to a file at the
// given `destination`.
func write(destination string) (err error) {
  file, err := os.Create(destination)

  if nil != err {
    return
  }

  defer file.Close()
  err = loader.Execute(file, templateData)

  return
}

// remove attempts to remove a file at the given `destination`.
func remove(destination string) (err error) {
  err = os.Remove(destination)
  return
}

// isGoferTaskFile checks an AST file's imports to make sure the
// file belongs to a package `tasks` and imports the gofer package.
func isGoferTaskFile(file *ast.File) bool {
  for _, imprt := range file.Imports {
    if PACKAGE_NAME == file.Name.String() && strings.ContainsAny(imprt.Path.Value, EXPECTED_IMPORT) {
      return true
    }
  }

  return false
}

// calculateDependencies determines the running order of a task
// and its dependencies, returning an error if the dependencies
// are cyclic or if a task couldn't be looked up..
func calculateDependencies(definition string) (definitions dependencies, err error) {
  half := make(dependencies, 0)
  marked := make(dependencies, 0)

  err = visitDefinition(definition, &half, &marked)

  if nil == err {
    definitions = marked
  }

  return
}

// visitDefinition helps calculateDependencies to resolve
// running order of its dependencies.
func visitDefinition(definition string, half, marked *dependencies) (err error) {
  if half.includes(definition) {
    return ErrCyclicDependency
  } else if !marked.includes(definition) && !half.includes(definition) {
    half.add(definition)
    task := gofer.index(definition)

    if nil == task {
      return ErrUnresolvableDependencies
    }

    for _, dependency := range task.Dependencies {
      err = visitDefinition(dependency, half, marked)
      if nil != err {
        return
      }
    }

    half.remove(definition)
    marked.add(definition)
  }

  return
}
