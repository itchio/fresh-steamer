// Command prune generates the pb package from a FileDescriptorSet, keeping
// only the messages and enums listed in an allowlist plus everything they
// transitively reference. The .proto files themselves stay verbatim mirrors of
// upstream; this is what keeps hundreds of unused Steam messages out of
// binaries that link fresh-steamer.
//
// Usage:
//
//	prune -desc all.pb -allowlist allowlist.txt -out ../pb -gopkg github.com/itchio/fresh-steamer/pb
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/pluginpb"
)

type entry struct {
	file   string
	parent string // full name of enclosing message, "" at top level
}

type index struct {
	msgs  map[string]entry
	enums map[string]entry
	defs  map[string]*descriptorpb.DescriptorProto
}

func main() {
	descPath := flag.String("desc", "", "FileDescriptorSet produced by protoc --descriptor_set_out --include_imports")
	allowPath := flag.String("allowlist", "", "file listing fully-qualified message/enum names to keep, one per line")
	outDir := flag.String("out", "", "directory to write generated .pb.go files into")
	goPkg := flag.String("gopkg", "", "Go import path for every generated file")
	flag.Parse()
	if *descPath == "" || *allowPath == "" || *outDir == "" || *goPkg == "" {
		flag.Usage()
		os.Exit(2)
	}

	raw, err := os.ReadFile(*descPath)
	if err != nil {
		log.Fatal(err)
	}
	var set descriptorpb.FileDescriptorSet
	if err := proto.Unmarshal(raw, &set); err != nil {
		log.Fatalf("parse descriptor set: %v", err)
	}

	idx := buildIndex(&set)
	roots, err := readAllowlist(*allowPath)
	if err != nil {
		log.Fatal(err)
	}
	keep := closure(idx, roots)

	var files []*descriptorpb.FileDescriptorProto
	var generate []string
	for _, fd := range set.File {
		pruned := pruneFile(fd, idx, keep)
		if pruned == nil {
			continue
		}
		files = append(files, pruned)
		generate = append(generate, pruned.GetName())
	}

	params := []string{"paths=source_relative"}
	for _, name := range generate {
		params = append(params, "M"+name+"="+*goPkg)
	}
	req := &pluginpb.CodeGeneratorRequest{
		FileToGenerate: generate,
		Parameter:      proto.String(strings.Join(params, ",")),
		ProtoFile:      files,
	}
	resp, err := runPlugin(req)
	if err != nil {
		log.Fatal(err)
	}
	if resp.Error != nil {
		log.Fatalf("protoc-gen-go: %s", resp.GetError())
	}
	for _, f := range resp.File {
		dst := filepath.Join(*outDir, f.GetName())
		if err := os.WriteFile(dst, []byte(f.GetContent()), 0o644); err != nil {
			log.Fatal(err)
		}
	}
	report(idx, keep, generate)
}

func buildIndex(set *descriptorpb.FileDescriptorSet) *index {
	idx := &index{msgs: map[string]entry{}, enums: map[string]entry{}, defs: map[string]*descriptorpb.DescriptorProto{}}
	var walk func(file, parent string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto)
	walk = func(file, parent string, msgs []*descriptorpb.DescriptorProto, enums []*descriptorpb.EnumDescriptorProto) {
		for _, e := range enums {
			idx.enums[parent+"."+e.GetName()] = entry{file, parent}
		}
		for _, m := range msgs {
			full := parent + "." + m.GetName()
			idx.msgs[full] = entry{file, parent}
			idx.defs[full] = m
			walk(file, full, m.NestedType, m.EnumType)
		}
	}
	for _, fd := range set.File {
		pkg := ""
		if fd.GetPackage() != "" {
			pkg = "." + fd.GetPackage()
		}
		walk(fd.GetName(), pkg, fd.MessageType, fd.EnumType)
	}
	return idx
}

func readAllowlist(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = strings.TrimSpace(line[:i])
		}
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, ".") {
			line = "." + line
		}
		out = append(out, line)
	}
	return out, sc.Err()
}

// closure returns every message and enum reachable from roots: field types,
// nested types they name, and the enclosing messages of any nested type.
func closure(idx *index, roots []string) map[string]bool {
	keep := map[string]bool{}
	var visit func(name string)
	visit = func(name string) {
		if keep[name] {
			return
		}
		ent, isMsg := idx.msgs[name]
		if !isMsg {
			var ok bool
			ent, ok = idx.enums[name]
			if !ok {
				log.Fatalf("allowlist: %q is not a message or enum in the descriptor set", name)
			}
		}
		keep[name] = true
		if ent.parent != "" {
			visit(ent.parent)
		}
		if !isMsg {
			return
		}
		for _, f := range idx.defs[name].Field {
			if f.GetTypeName() != "" {
				visit(f.GetTypeName())
			}
		}
	}
	for _, r := range roots {
		visit(r)
	}
	return keep
}

// pruneFile returns a copy of fd containing only kept types, or nil if nothing
// in it is kept. Services, extensions and custom options are dropped, and the
// import list is recomputed from what the kept fields actually reference.
func pruneFile(fd *descriptorpb.FileDescriptorProto, idx *index, keep map[string]bool) *descriptorpb.FileDescriptorProto {
	pkg := ""
	if fd.GetPackage() != "" {
		pkg = "." + fd.GetPackage()
	}
	deps := map[string]bool{}
	out := proto.Clone(fd).(*descriptorpb.FileDescriptorProto)
	out.MessageType = pruneMessages(pkg, out.MessageType, idx, keep, fd.GetName(), deps)
	out.EnumType = pruneEnums(pkg, out.EnumType, keep)
	if len(out.MessageType) == 0 && len(out.EnumType) == 0 {
		return nil
	}
	out.Service = nil
	out.Extension = nil
	out.PublicDependency = nil
	out.WeakDependency = nil
	out.SourceCodeInfo = nil
	stripUnknown(out.Options)
	out.Dependency = nil
	for d := range deps {
		out.Dependency = append(out.Dependency, d)
	}
	sort.Strings(out.Dependency)
	return out
}

func pruneMessages(parent string, msgs []*descriptorpb.DescriptorProto, idx *index, keep map[string]bool, file string, deps map[string]bool) []*descriptorpb.DescriptorProto {
	var out []*descriptorpb.DescriptorProto
	for _, m := range msgs {
		full := parent + "." + m.GetName()
		if !keep[full] {
			continue
		}
		m.NestedType = pruneMessages(full, m.NestedType, idx, keep, file, deps)
		m.EnumType = pruneEnums(full, m.EnumType, keep)
		m.Extension = nil
		stripUnknown(m.Options)
		for _, f := range m.Field {
			stripUnknown(f.Options)
			if tn := f.GetTypeName(); tn != "" {
				ent, ok := idx.msgs[tn]
				if !ok {
					ent = idx.enums[tn]
				}
				if ent.file != file {
					deps[ent.file] = true
				}
			}
		}
		for _, o := range m.OneofDecl {
			stripUnknown(o.Options)
		}
		out = append(out, m)
	}
	return out
}

func pruneEnums(parent string, enums []*descriptorpb.EnumDescriptorProto, keep map[string]bool) []*descriptorpb.EnumDescriptorProto {
	var out []*descriptorpb.EnumDescriptorProto
	for _, e := range enums {
		if !keep[parent+"."+e.GetName()] {
			continue
		}
		stripUnknown(e.Options)
		for _, v := range e.Value {
			stripUnknown(v.Options)
		}
		out = append(out, e)
	}
	return out
}

// stripUnknown drops custom options (Steam's description/msgpool extensions),
// which arrive as unknown fields since their extension types aren't linked in.
func stripUnknown(m proto.Message) {
	if m == nil || !m.ProtoReflect().IsValid() {
		return
	}
	m.ProtoReflect().SetUnknown(nil)
}

func runPlugin(req *pluginpb.CodeGeneratorRequest) (*pluginpb.CodeGeneratorResponse, error) {
	in, err := proto.Marshal(req)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("protoc-gen-go")
	cmd.Stdin = strings.NewReader(string(in))
	cmd.Stderr = os.Stderr
	outBytes, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("run protoc-gen-go: %w", err)
	}
	var resp pluginpb.CodeGeneratorResponse
	if err := proto.Unmarshal(outBytes, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

func report(idx *index, keep map[string]bool, generated []string) {
	msgs, enums := 0, 0
	for name := range keep {
		if _, ok := idx.msgs[name]; ok {
			msgs++
		} else {
			enums++
		}
	}
	fmt.Fprintf(os.Stderr, "prune: kept %d/%d messages, %d/%d enums across %d files\n",
		msgs, len(idx.msgs), enums, len(idx.enums), len(generated))
}
