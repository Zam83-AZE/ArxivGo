package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// --- MODELLƏR ---

type FileData struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Path         string    `json:"path"`
	VFolder      string    `json:"vFolder"`
	Tags         []string  `json:"tags"`
	Reads        int       `json:"reads"`
	CreatedAt    time.Time `json:"createdAt"`
	LastAccessed time.Time `json:"lastAccessed"`
}

type AppState struct {
	Files []FileData `json:"files"`
	mu    sync.RWMutex
}

type MetaResponse struct {
	Recents        []FileData `json:"recents"`
	VirtualFolders []string   `json:"folders"`
	TotalFiles     int        `json:"totalFiles"`
}

type NoteRequest struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	VFolder string   `json:"vFolder"`
}

type UpdateContentRequest struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

type APIResponse struct {
	Success   bool   `json:"success"`
	Message   string `json:"message"`
	Versioned bool   `json:"versioned"`
	Moved     bool   `json:"moved"`
	FileName  string `json:"fileName"`
}

const dbPath = "data.json"
const storageFolder = "ArxivGo_Storage"
var state AppState

// --- KÖMƏKÇİ FUNKSİYALAR ---

func sendJSON(w http.ResponseWriter, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func getNextVersion(dir, filename string) (string, string) {
	ext := filepath.Ext(filename)
	base := strings.TrimSuffix(filename, ext)

	version := 2
	if idx := strings.LastIndex(base, "_v"); idx != -1 {
		if v, err := strconv.Atoi(base[idx+2:]); err == nil {
			version = v + 1
			base = base[:idx]
		}
	}

	for {
		newName := fmt.Sprintf("%s_v%d%s", base, version, ext)
		newPath := filepath.Join(dir, newName)
		if _, err := os.Stat(newPath); os.IsNotExist(err) {
			return newName, newPath
		}
		version++
	}
}

// --- BACKEND MƏNTİQİ ---

func initStorage() {
	if _, err := os.Stat(storageFolder); os.IsNotExist(err) {
		os.MkdirAll(storageFolder, 0755)
	}
}

func loadDB() {
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, err := os.Stat(dbPath); os.IsNotExist(err) {
		state.Files = []FileData{}
		return
	}
	data, err := os.ReadFile(dbPath)
	if err == nil {
		json.Unmarshal(data, &state.Files)
	}
}

func saveDB() {
	state.mu.RLock()
	dataCopy := make([]FileData, len(state.Files))
	copy(dataCopy, state.Files)
	state.mu.RUnlock()

	data, _ := json.Marshal(dataCopy)
	os.WriteFile(dbPath, data, 0644)
}

func performScan(pathsToScan []string) {
	state.mu.RLock()
	existingPaths := make(map[string]bool, len(state.Files))
	for _, f := range state.Files {
		existingPaths[f.Path] = true
	}
	state.mu.RUnlock()

	var newFiles []FileData

	for _, dir := range pathsToScan {
		filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil { return nil }
			if d.IsDir() {
				name := d.Name()
				if name == ".git" || name == "node_modules" || name == "Windows" || name == "AppData" || name == "sys" {
					return filepath.SkipDir
				}
				return nil
			}
			if !existingPaths[path] {
				hash := md5.Sum([]byte(path))
				uniqueID := hex.EncodeToString(hash[:])

				newFiles = append(newFiles, FileData{
					ID:        uniqueID,
					Name:      d.Name(),
					Path:      path,
					Tags:      []string{},
					CreatedAt: time.Now(),
				})
				existingPaths[path] = true
			}
			return nil
		})
	}

	if len(newFiles) > 0 {
		state.mu.Lock()
		state.Files = append(state.Files, newFiles...)
		state.mu.Unlock()
		go saveDB()
		fmt.Printf("✅ Skan bitdi: %d yeni fayl tapıldı!\n", len(newFiles))
	}
}

func autoStartupScan() {
	var pathsToScan []string
	pathsToScan = append(pathsToScan, "./")

	if runtime.GOOS == "windows" {
		for c := 'A'; c <= 'Z'; c++ {
			drivePath := string(c) + ":\\"
			if _, err := os.Stat(drivePath); err == nil {
				pathsToScan = append(pathsToScan, drivePath)
			}
		}
	} else {
		if _, err := os.Stat("/workspaces"); err == nil {
			pathsToScan = append(pathsToScan, "/workspaces")
		} else {
			home, _ := os.UserHomeDir()
			pathsToScan = append(pathsToScan, filepath.Join(home, "Documents"), filepath.Join(home, "Desktop"), filepath.Join(home, "Downloads"))
		}
	}
	performScan(pathsToScan)
}

// --- API HANDLERS ---

func searchHandler(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(r.URL.Query().Get("q"))
	folderFilter := r.URL.Query().Get("folder")
	
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 { limit = 50 } 
	
	var tagMatches []FileData
	var nameMatches []FileData
	
	targetCount := offset + limit
	
	state.mu.RLock()
	defer state.mu.RUnlock()

	for _, f := range state.Files {
		if folderFilter != "" && f.VFolder != folderFilter {
			continue
		}
		if query == "" {
			if len(nameMatches) < targetCount {
				nameMatches = append(nameMatches, f)
			}
			if len(nameMatches) >= targetCount { break }
			continue
		}

		isTagMatch := false
		for _, t := range f.Tags {
			if strings.Contains(strings.ToLower(t), query) {
				isTagMatch = true
				break
			}
		}

		if isTagMatch {
			if len(tagMatches) < targetCount {
				tagMatches = append(tagMatches, f)
			}
		} else {
			if strings.Contains(strings.ToLower(f.Name), query) {
				if len(nameMatches) < targetCount {
					nameMatches = append(nameMatches, f)
				}
			}
		}

		if len(tagMatches) >= targetCount && len(nameMatches) >= targetCount {
			break
		}
	}

	var allMatches []FileData
	allMatches = append(allMatches, tagMatches...)
	allMatches = append(allMatches, nameMatches...)

	results := []FileData{}
	if offset < len(allMatches) {
		end := offset + limit
		if end > len(allMatches) {
			end = len(allMatches)
		}
		results = allMatches[offset:end]
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func metaHandler(w http.ResponseWriter, r *http.Request) {
	state.mu.RLock()
	defer state.mu.RUnlock()

	foldersMap := make(map[string]bool)
	var recents []FileData
	var recentCandidates []FileData

	if len(state.Files) > 100 {
		recentCandidates = make([]FileData, 100)
		copy(recentCandidates, state.Files[len(state.Files)-100:])
	} else {
		recentCandidates = make([]FileData, len(state.Files))
		copy(recentCandidates, state.Files)
	}

	sort.Slice(recentCandidates, func(i, j int) bool {
		return recentCandidates[i].CreatedAt.After(recentCandidates[j].CreatedAt)
	})

	if len(recentCandidates) > 10 {
		recents = recentCandidates[:10]
	} else {
		recents = recentCandidates
	}

	for _, f := range state.Files {
		if f.VFolder != "" {
			foldersMap[f.VFolder] = true
		}
	}

	var folders []string
	for k := range foldersMap {
		folders = append(folders, k)
	}

	resp := MetaResponse{
		Recents:        recents,
		VirtualFolders: folders,
		TotalFiles:     len(state.Files),
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func updateHandler(w http.ResponseWriter, r *http.Request) {
	var updated FileData
	if err := json.NewDecoder(r.Body).Decode(&updated); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	isMoved := false
	finalName := updated.Name

	state.mu.Lock()
	for i, f := range state.Files {
		if f.ID == updated.ID {
			state.Files[i].Tags = updated.Tags
			
			if f.VFolder != updated.VFolder {
				oldPath := f.Path
				
				newDir := storageFolder
				if updated.VFolder != "" {
					newDir = filepath.Join(storageFolder, filepath.FromSlash(updated.VFolder))
				}
				os.MkdirAll(newDir, 0755) 
				
				newPath := filepath.Join(newDir, filepath.Base(oldPath))
				
				if oldPath != newPath {
					err := os.Rename(oldPath, newPath)
					if err != nil {
						input, errRead := os.ReadFile(oldPath)
						if errRead == nil {
							errWrite := os.WriteFile(newPath, input, 0644)
							if errWrite == nil {
								os.Remove(oldPath)
								state.Files[i].Path = newPath
								isMoved = true
							}
						}
					} else {
						state.Files[i].Path = newPath
						isMoved = true
					}
				}
			}
			
			state.Files[i].VFolder = updated.VFolder
			finalName = state.Files[i].Name
			break
		}
	}
	state.mu.Unlock()
	go saveDB() 

	sendJSON(w, APIResponse{Success: true, Moved: isMoved, FileName: finalName})
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(500 << 20)
	if err != nil { http.Error(w, err.Error(), 400); return }

	file, handler, err := r.FormFile("file")
	if err != nil { http.Error(w, err.Error(), 400); return }
	defer file.Close()

	tagsJSON := r.FormValue("tags")
	vFolder := r.FormValue("vFolder")

	destDir := storageFolder
	if vFolder != "" {
		destDir = filepath.Join(storageFolder, filepath.FromSlash(vFolder))
		os.MkdirAll(destDir, 0755)
	}

	finalName := handler.Filename
	destPath := filepath.Join(destDir, finalName)

	isVersioned := false
	info, err := os.Stat(destPath)
	if err == nil {
		if info.Size() != handler.Size {
			finalName, destPath = getNextVersion(destDir, handler.Filename)
			isVersioned = true
		}
	}

	destFile, err := os.Create(destPath)
	if err != nil { http.Error(w, err.Error(), 500); return }
	defer destFile.Close()

	io.Copy(destFile, file)

	var tags []string
	if tagsJSON != "" {
		json.Unmarshal([]byte(tagsJSON), &tags)
	}

	absPath, _ := filepath.Abs(destPath)
	hash := md5.Sum([]byte(absPath))
	uniqueID := hex.EncodeToString(hash[:])

	state.mu.Lock()
	existsInDB := false
	for i, f := range state.Files {
		if f.Path == absPath {
			state.Files[i].Tags = tags
			state.Files[i].VFolder = vFolder
			state.Files[i].CreatedAt = time.Now()
			existsInDB = true
			break
		}
	}

	if !existsInDB {
		newFile := FileData{
			ID:        uniqueID,
			Name:      finalName,
			Path:      absPath,
			VFolder:   vFolder,
			Tags:      tags,
			CreatedAt: time.Now(),
		}
		state.Files = append(state.Files, newFile)
	}
	state.mu.Unlock()
	go saveDB()

	sendJSON(w, APIResponse{Success: true, Versioned: isVersioned, FileName: finalName})
}

func createNoteHandler(w http.ResponseWriter, r *http.Request) {
	var req NoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	safeTitle := strings.ReplaceAll(req.Title, " ", "_")
	safeTitle = strings.ReplaceAll(safeTitle, "/", "-")
	safeTitle = strings.ReplaceAll(safeTitle, "\\", "-")
	if safeTitle == "" { safeTitle = "Adsiz_Qeyd" }
	
	fileName := safeTitle + ".txt"
	destDir := storageFolder
	if req.VFolder != "" {
		destDir = filepath.Join(storageFolder, filepath.FromSlash(req.VFolder))
		os.MkdirAll(destDir, 0755)
	}
	
	destPath := filepath.Join(destDir, fileName)

	isVersioned := false
	if _, err := os.Stat(destPath); err == nil {
		fileName, destPath = getNextVersion(destDir, fileName)
		isVersioned = true
	}

	err := os.WriteFile(destPath, []byte(req.Content), 0644)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	absPath, _ := filepath.Abs(destPath)
	hash := md5.Sum([]byte(absPath))
	uniqueID := hex.EncodeToString(hash[:])

	newFile := FileData{
		ID:        uniqueID,
		Name:      fileName,
		Path:      absPath,
		VFolder:   req.VFolder,
		Tags:      req.Tags,
		CreatedAt: time.Now(),
	}

	state.mu.Lock()
	state.Files = append(state.Files, newFile)
	state.mu.Unlock()
	go saveDB()

	sendJSON(w, APIResponse{Success: true, Versioned: isVersioned, FileName: fileName})
}

func scanHandler(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir != "" { 
		go performScan([]string{dir}) 
	}
	fmt.Fprint(w, "Scan initiated")
}

func openFileHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	state.mu.Lock()
	var targetPath string
	for i, f := range state.Files {
		if f.ID == id {
			state.Files[i].Reads++
			state.Files[i].LastAccessed = time.Now()
			targetPath = f.Path
			break
		}
	}
	state.mu.Unlock()
	go saveDB()

	if targetPath != "" {
		switch runtime.GOOS {
		case "windows": 
			exec.Command("cmd", "/C", "start", "", targetPath).Start()
			fmt.Fprint(w, "Opened on Windows")
		case "darwin":  
			exec.Command("open", targetPath).Start()
			fmt.Fprint(w, "Opened on Mac")
		default:        
			exec.Command("xdg-open", targetPath).Start()
			fmt.Fprint(w, "Opened on Linux")
		}
	} else {
		http.Error(w, "Fayl tapılmadı", http.StatusNotFound)
	}
}

func downloadHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	state.mu.Lock()
	var targetPath string
	var fileName string
	for i, f := range state.Files {
		if f.ID == id {
			state.Files[i].Reads++
			state.Files[i].LastAccessed = time.Now()
			targetPath = f.Path
			fileName = f.Name
			break
		}
	}
	state.mu.Unlock()
	go saveDB()

	if targetPath != "" {
		ext := strings.ToLower(filepath.Ext(fileName))
		if ext == ".pdf" || ext == ".png" || ext == ".jpg" || ext == ".jpeg" || ext == ".txt" || ext == ".mp4" || ext == ".webp" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=\"%s\"", fileName))
		} else {
			w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", fileName))
		}
		http.ServeFile(w, r, targetPath)
	} else {
		http.Error(w, "Fayl tapılmadı", http.StatusNotFound)
	}
}

func getFileContentHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	state.mu.RLock()
	var targetPath string
	for _, f := range state.Files {
		if f.ID == id {
			targetPath = f.Path
			break
		}
	}
	state.mu.RUnlock()

	if targetPath != "" {
		content, err := os.ReadFile(targetPath)
		if err == nil {
			w.Write(content)
			return
		}
	}
	http.Error(w, "Fayl tapılmadı və ya oxunmadı", http.StatusNotFound)
}

func updateFileContentHandler(w http.ResponseWriter, r *http.Request) {
	var req UpdateContentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	state.mu.RLock()
	var existingFile FileData
	var fileIndex int
	found := false
	for i, f := range state.Files {
		if f.ID == req.ID {
			existingFile = f
			fileIndex = i
			found = true
			break
		}
	}
	state.mu.RUnlock()

	if !found {
		http.Error(w, "Fayl tapılmadı", http.StatusNotFound)
		return
	}

	info, err := os.Stat(existingFile.Path)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	newContent := []byte(req.Content)
	isVersioned := false
	var finalName, finalPath string

	if info.Size() != int64(len(newContent)) {
		finalName, finalPath = getNextVersion(filepath.Dir(existingFile.Path), existingFile.Name)
		isVersioned = true
	} else {
		finalName = existingFile.Name
		finalPath = existingFile.Path
	}

	err = os.WriteFile(finalPath, newContent, 0644)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	state.mu.Lock()
	if isVersioned {
		absPath, _ := filepath.Abs(finalPath)
		hash := md5.Sum([]byte(absPath))
		uniqueID := hex.EncodeToString(hash[:])

		newFile := FileData{
			ID:           uniqueID,
			Name:         finalName,
			Path:         absPath,
			VFolder:      existingFile.VFolder,
			Tags:         existingFile.Tags,
			Reads:        0,
			CreatedAt:    time.Now(),
			LastAccessed: time.Now(),
		}
		state.Files = append(state.Files, newFile)
	} else {
		state.Files[fileIndex].LastAccessed = time.Now()
		state.Files[fileIndex].CreatedAt = time.Now() 
	}
	state.mu.Unlock()

	go saveDB() 
	sendJSON(w, APIResponse{Success: true, Versioned: isVersioned, FileName: finalName})
}

func main() {
	initStorage()
	loadDB()
	go autoStartupScan()

	http.HandleFunc("/api/search", searchHandler) 
	http.HandleFunc("/api/meta", metaHandler)     
	http.HandleFunc("/api/update", updateHandler)
	http.HandleFunc("/api/upload", uploadHandler)
	http.HandleFunc("/api/create-note", createNoteHandler)
	http.HandleFunc("/api/scan", scanHandler)
	http.HandleFunc("/api/open", openFileHandler)
	http.HandleFunc("/api/download", downloadHandler) 
	
	http.HandleFunc("/api/get-content", getFileContentHandler)
	http.HandleFunc("/api/update-content", updateFileContentHandler)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, uiHTML)
	})

	port := 8080
	var listener net.Listener
	var err error

	for {
		listener, err = net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err == nil {
			break 
		}
		port++ 
	}

	fmt.Printf("🚀 Server %d portunda hazırdır. UI: http://localhost:%d\n", port, port)
	log.Fatal(http.Serve(listener, nil))
}

// --- UI (FRONTEND) ---
const uiHTML = `
<!DOCTYPE html>
<html lang="az">
<head>
    <meta charset="UTF-8">
    <title>ArxivGo | Pro Edition</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://unpkg.com/vue@3/dist/vue.global.js"></script>
    <script src="https://unpkg.com/lucide@latest"></script>
    <style>
        @import url('https://fonts.googleapis.com/css2?family=Inter:wght@300;400;600&display=swap');
        body { font-family: 'Inter', sans-serif; background-color: #fff; color: #1e293b; margin:0; padding:0; overflow: hidden; }
        .search-container:focus-within { ring: 4px solid #eff6ff; border-color: #3b82f6; }
        .modal-overlay { background: rgba(255, 255, 255, 0.98); backdrop-filter: blur(10px); z-index: 100; }
        .custom-scroll::-webkit-scrollbar { width: 6px; }
        .custom-scroll::-webkit-scrollbar-thumb { background: #cbd5e1; border-radius: 10px; }
        .drop-zone-active { z-index: 9999; opacity: 1; pointer-events: all; }
        .drop-zone-inactive { z-index: -1; opacity: 0; pointer-events: none; }
        
        /* Toast Animasiyaları */
        .toast-enter-active, .toast-leave-active { transition: all 0.4s cubic-bezier(0.175, 0.885, 0.32, 1.275); }
        .toast-enter-from { opacity: 0; transform: translateX(50px); }
        .toast-leave-to { opacity: 0; transform: translateX(50px); }
    </style>
</head>
<body class="w-screen h-screen relative">
    
    <div id="app" class="w-full h-full flex flex-col items-center justify-center relative bg-white">
        
        <div class="fixed bottom-6 right-6 z-[9999] flex flex-col gap-3 pointer-events-none">
            <transition-group name="toast">
                <div v-for="toast in toasts" :key="toast.id" 
                     class="bg-slate-800 text-white px-5 py-4 rounded-2xl shadow-2xl flex items-center gap-4 text-sm font-medium w-80 pointer-events-auto border border-slate-700">
                    <div class="bg-blue-500/20 p-2 rounded-full text-blue-400">
                        <i data-lucide="bell" class="w-5 h-5"></i>
                    </div>
                    <div class="flex-1 break-words leading-snug">{{ toast.message }}</div>
                </div>
            </transition-group>
        </div>

        <div :class="dragActive ? 'drop-zone-active' : 'drop-zone-inactive'" 
             class="fixed inset-0 bg-blue-50/90 border-[6px] border-dashed border-blue-400 flex items-center justify-center transition-opacity duration-200">
            <div class="text-3xl font-bold text-blue-600 bg-white px-10 py-6 rounded-3xl shadow-2xl flex items-center gap-4">
                <i data-lucide="upload-cloud" class="w-12 h-12"></i> Faylı buraya buraxın
            </div>
        </div>

        <div class="w-full max-w-2xl px-6 relative z-10 flex flex-col">
            <div v-if="!activeModal && !editingFile && !uploadingFile" class="text-center w-full">
                <h1 class="text-5xl font-light mb-1 select-none">Arxiv<span class="font-bold text-blue-600">Go</span></h1>
                <p class="text-[10px] text-slate-400 font-bold uppercase tracking-widest mb-10">Cəmi Fayl: {{ totalFiles }}</p>

                <div class="flex justify-center gap-3 mb-8">
                    <button @click="openModal('folders')" class="px-5 py-2 rounded-xl bg-slate-50 text-slate-600 text-xs font-bold hover:bg-slate-100 transition uppercase tracking-widest border border-slate-100">Qovluqlar</button>
                    <button @click="openModal('recents')" class="px-5 py-2 rounded-xl bg-slate-50 text-slate-600 text-xs font-bold hover:bg-slate-100 transition uppercase tracking-widest border border-slate-100">Son Əlavələr</button>
                    <button @click="openModal('note')" class="px-5 py-2 rounded-xl bg-green-50 text-green-600 text-xs font-bold hover:bg-green-100 transition uppercase tracking-widest border border-green-100 flex items-center gap-2"><i data-lucide="plus-circle" class="w-4 h-4"></i> Yeni Qeyd</button>
                </div>

                <div class="relative w-full mb-8">
                    <div class="search-container flex items-center bg-white border border-slate-200 rounded-full px-6 py-4 transition-all shadow-sm relative z-20">
                        <i data-lucide="search" class="w-5 h-5 text-slate-400 mr-4"></i>
                        <input v-model="query" @input="onSearchInput" type="text" placeholder="Axtar və ya faylı ekrana at..." class="flex-1 outline-none text-lg bg-transparent">
                        <div v-if="isSearching" class="w-4 h-4 border-2 border-blue-500 border-t-transparent rounded-full animate-spin"></div>
                    </div>

                    <div v-if="searchResults.length > 0" 
                         @scroll="handleScroll"
                         class="absolute w-full left-0 mt-2 bg-white border border-slate-100 rounded-3xl shadow-2xl z-50 overflow-hidden text-left flex flex-col custom-scroll"
                         style="max-height: 45vh; overflow-y: auto;">
                        
                        <div v-for="f in searchResults" :key="f.id" class="px-6 py-4 hover:bg-blue-50/50 flex justify-between items-center group cursor-pointer border-b border-slate-50 last:border-0 shrink-0">
                            <div @click="openFile(f)" class="flex-1 flex items-center gap-4">
                                <i data-lucide="file" class="w-5 h-5 text-slate-300 flex-shrink-0"></i>
                                <div>
                                    <div class="text-sm font-medium break-words">{{ f.name }}</div>
                                    <div class="flex gap-2 mt-1 flex-wrap">
                                        <span v-for="t in f.tags" class="text-[9px] bg-blue-50 text-blue-500 px-2 py-0.5 rounded font-bold uppercase tracking-tighter">#{{ t }}</span>
                                        <span v-if="f.vFolder" class="text-[9px] bg-slate-100 text-slate-500 px-2 py-0.5 rounded font-bold uppercase tracking-tighter">{{ f.vFolder }}</span>
                                    </div>
                                </div>
                            </div>
                            <button @click.stop="startEdit(f)" class="p-2 opacity-0 group-hover:opacity-100 hover:bg-white shadow-sm rounded-full transition text-blue-600 flex-shrink-0">
                                <i data-lucide="edit-3" class="w-4 h-4"></i>
                            </button>
                        </div>

                        <div v-if="isLoadingMore" class="py-4 text-center text-xs font-bold text-slate-400 flex justify-center items-center gap-2">
                            <div class="w-3 h-3 border-2 border-slate-400 border-t-transparent rounded-full animate-spin"></div> Yüklənir...
                        </div>
                        
                        <div v-if="!hasMore && searchResults.length > 0" class="py-3 text-center text-[10px] text-slate-300 uppercase tracking-widest font-bold bg-slate-50 border-t border-slate-100">
                            Siyahının sonu
                        </div>
                    </div>
                </div>
            </div>
        </div>

        <div v-if="uploadingFile" class="fixed inset-0 modal-overlay flex items-center justify-center p-6 z-[1000]">
            <div class="w-full max-w-md bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10">
                <div class="flex items-center gap-3 text-blue-600 mb-2">
                    <i data-lucide="file-plus" class="w-6 h-6 flex-shrink-0"></i>
                    <h3 class="text-xl font-bold break-words">{{ uploadingFile.name }}</h3>
                </div>
                <p class="text-[10px] text-slate-400 uppercase tracking-widest mb-8">Yeni Fayl Əlavə Olunur</p>

                <div class="mb-6">
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Teqlər əlavə et (Enter)</label>
                    <div class="flex flex-wrap gap-2 p-3 bg-slate-50 rounded-2xl border border-slate-100">
                        <span v-for="t in uploadTags" @click="uploadTags = uploadTags.filter(tag => tag !== t)" class="bg-white text-blue-600 px-3 py-1 rounded-full text-xs font-bold border border-blue-100 cursor-pointer hover:bg-red-50 hover:text-red-500 hover:border-red-100 transition">#{{ t }}</span>
                        <input v-model="newTag" @keyup.enter="addUploadTag" placeholder="..." class="bg-transparent outline-none text-xs flex-1">
                    </div>
                </div>

                <div class="mb-10">
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Qovluq Seç və ya Yaz</label>
                    <div class="flex flex-wrap gap-2 mb-3" v-if="virtualFolders.length > 0">
                        <span v-for="v in virtualFolders" @click="uploadVFolder = v" class="text-[10px] bg-blue-50 text-blue-600 px-3 py-1.5 rounded-lg cursor-pointer hover:bg-blue-600 hover:text-white transition font-bold border border-blue-100">{{ v }}</span>
                    </div>
                    <input v-model="uploadVFolder" placeholder="Məsələn: Sənədlər/Hesabatlar..." class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-blue-100">
                </div>

                <div class="flex gap-3">
                    <button @click="confirmUpload" class="flex-1 bg-blue-600 text-white py-4 rounded-2xl font-bold hover:bg-blue-700 transition">Sistemə Əlavə Et</button>
                    <button @click="uploadingFile = null" class="px-8 py-4 bg-slate-100 text-slate-400 rounded-2xl font-bold hover:bg-slate-200 transition">Ləğv Et</button>
                </div>
            </div>
        </div>

        <div v-if="activeModal === 'note'" class="fixed inset-0 modal-overlay flex items-center justify-center p-6 z-[1000]">
            <div class="w-full max-w-xl bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10 flex flex-col max-h-[90vh]">
                <div class="flex justify-between items-center mb-6">
                    <h2 class="text-2xl font-bold flex items-center gap-2 text-green-600"><i data-lucide="edit"></i> Yeni Qeyd</h2>
                    <button @click="activeModal = null" class="p-2 hover:bg-slate-100 rounded-full transition"><i data-lucide="x"></i></button>
                </div>

                <div class="flex-1 overflow-y-auto custom-scroll pr-2">
                    <div class="mb-4">
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Başlıq</label>
                        <input v-model="noteTitle" type="text" placeholder="Qeydin adı..." class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-green-100">
                    </div>

                    <div class="mb-4">
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Məzmun (Mətn)</label>
                        <textarea v-model="noteContent" rows="6" placeholder="Qeydinizi buraya yazın..." class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-green-100 resize-none"></textarea>
                    </div>

                    <div class="mb-4">
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Teqlər</label>
                        <div class="flex flex-wrap gap-2 p-3 bg-slate-50 rounded-2xl border border-slate-100">
                            <span v-for="t in uploadTags" @click="uploadTags = uploadTags.filter(tag => tag !== t)" class="bg-white text-green-600 px-3 py-1 rounded-full text-xs font-bold border border-green-100 cursor-pointer hover:bg-red-50 hover:text-red-500 hover:border-red-100 transition">#{{ t }}</span>
                            <input v-model="newTag" @keyup.enter="addUploadTag" placeholder="..." class="bg-transparent outline-none text-xs flex-1">
                        </div>
                    </div>

                    <div class="mb-6">
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Qovluq Seç və ya Yaz</label>
                        <div class="flex flex-wrap gap-2 mb-3" v-if="virtualFolders.length > 0">
                            <span v-for="v in virtualFolders" @click="uploadVFolder = v" class="text-[10px] bg-green-50 text-green-600 px-3 py-1.5 rounded-lg cursor-pointer hover:bg-green-600 hover:text-white transition font-bold border border-green-100">{{ v }}</span>
                        </div>
                        <input v-model="uploadVFolder" placeholder="Məsələn: Fikirlər..." class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-green-100">
                    </div>
                </div>

                <div class="flex gap-3 mt-4">
                    <button @click="saveNote" class="flex-1 bg-green-600 text-white py-4 rounded-2xl font-bold hover:bg-green-700 transition">Qeydi Yarat</button>
                    <button @click="activeModal = null" class="px-8 py-4 bg-slate-100 text-slate-400 rounded-2xl font-bold hover:bg-slate-200 transition">Ləğv Et</button>
                </div>
            </div>
        </div>

        <div v-if="activeModal === 'edit-text'" class="fixed inset-0 modal-overlay flex items-center justify-center p-6 z-[1000]">
            <div class="w-full max-w-4xl bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10 flex flex-col h-[90vh]">
                <div class="flex justify-between items-center mb-6">
                    <h2 class="text-2xl font-bold flex items-center gap-2 text-blue-600"><i data-lucide="file-edit"></i> Mətn Redaktoru</h2>
                    <button @click="activeModal = null" class="p-2 hover:bg-slate-100 rounded-full transition"><i data-lucide="x"></i></button>
                </div>

                <div class="mb-4">
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Faylın Adı</label>
                    <input disabled v-model="noteTitle" type="text" class="w-full p-4 bg-slate-100 text-slate-500 rounded-2xl border-none outline-none cursor-not-allowed font-medium break-words">
                </div>

                <div class="flex-1 flex flex-col min-h-0 mb-6">
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Məzmun (Dəyişdirə Bilərsiniz)</label>
                    <textarea v-model="noteContent" class="flex-1 w-full p-6 bg-slate-50 rounded-2xl border border-slate-200 outline-none focus:ring-2 ring-blue-200 resize-none font-mono text-sm custom-scroll leading-relaxed text-slate-700"></textarea>
                </div>

                <div class="flex gap-3 mt-auto">
                    <button @click="saveTextContent" class="flex-1 bg-blue-600 text-white py-4 rounded-2xl font-bold hover:bg-blue-700 transition shadow-lg shadow-blue-200">
                        Dəyişikliyi Fayla Yaz (Save)
                    </button>
                    <button @click="downloadCurrentTextFile" class="px-6 py-4 bg-slate-50 border border-slate-200 text-slate-600 rounded-2xl font-bold hover:bg-slate-100 transition flex items-center gap-2">
                        <i data-lucide="download" class="w-5 h-5"></i> Yüklə
                    </button>
                    <button @click="activeModal = null" class="px-8 py-4 bg-slate-100 text-slate-500 rounded-2xl font-bold hover:bg-slate-200 transition">Bağla</button>
                </div>
            </div>
        </div>

        <div v-if="(activeModal === 'folders' || activeModal === 'recents') && !editingFile && !uploadingFile" class="fixed inset-0 modal-overlay flex items-center justify-center p-6 z-[1000]">
            <div class="w-full max-w-xl bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10 flex flex-col max-h-[80vh]">
                <div class="flex justify-between items-center mb-8">
                    <h2 class="text-2xl font-bold capitalize">{{ activeModal === 'folders' ? 'Qovluqlar' : 'Son Əlavələr' }}</h2>
                    <button @click="activeModal = null" class="p-2 hover:bg-slate-100 rounded-full transition"><i data-lucide="x"></i></button>
                </div>
                <div class="flex-1 overflow-y-auto custom-scroll">
                    <div v-if="activeModal === 'folders'" class="grid grid-cols-2 gap-3">
                        <div v-for="v in virtualFolders" @click="searchFolder(v)" class="p-5 bg-slate-50 rounded-2xl border border-slate-100 hover:border-blue-400 hover:bg-blue-50 cursor-pointer transition">
                            <i data-lucide="folder" class="w-5 h-5 text-blue-500 mb-2"></i>
                            <div class="text-sm font-bold text-slate-700 break-words">{{ v || 'Adsız' }}</div>
                        </div>
                    </div>
                    <div v-if="activeModal === 'recents'" class="space-y-3">
                        <div v-for="f in recents" @click="openFile(f)" class="p-4 bg-slate-50 rounded-2xl flex justify-between items-center cursor-pointer hover:bg-blue-50 transition">
                            <span class="text-sm font-medium break-words flex-1 pr-4">{{ f.name }}</span>
                            <span class="text-[10px] text-slate-400 font-bold uppercase flex-shrink-0">{{ formatDate(f.createdAt) }}</span>
                        </div>
                    </div>
                </div>
            </div>
        </div>

        <div v-if="editingFile" class="fixed inset-0 modal-overlay flex items-center justify-center p-6 z-[1000]">
            <div class="w-full max-w-md bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10 flex flex-col max-h-[90vh]">
                <div class="overflow-y-auto custom-scroll pr-2 mb-4">
                    <h3 class="text-xl font-bold mb-2 break-words">{{ editingFile.name }}</h3>
                    <p class="text-[10px] text-slate-400 uppercase tracking-widest mb-8">Fayl Redaktəsi</p>

                    <div class="mb-6">
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Teqlər</label>
                        <div class="flex flex-wrap gap-2 p-3 bg-slate-50 rounded-2xl border border-slate-100">
                            <span v-for="t in editingFile.tags" @click="removeTag(t)" class="bg-white text-blue-600 px-3 py-1 rounded-full text-xs font-bold border border-blue-100 cursor-pointer hover:bg-red-50 hover:text-red-500 hover:border-red-100 transition">#{{ t }}</span>
                            <input v-model="newTag" @keyup.enter="addTag" placeholder="..." class="bg-transparent outline-none text-xs flex-1">
                        </div>
                    </div>

                    <div class="mb-4">
                        <label class="text-[10px] font-bold text-blue-500 uppercase block mb-2">Qovluq (Mövcudu seç və ya köçür)</label>
                        <div class="flex flex-wrap gap-2 mb-3" v-if="virtualFolders.length > 0">
                            <span v-for="v in virtualFolders" @click="editingFile.vFolder = v" class="text-[10px] bg-blue-50 text-blue-600 px-3 py-1.5 rounded-lg cursor-pointer hover:bg-blue-600 hover:text-white transition font-bold border border-blue-100">{{ v }}</span>
                        </div>
                        <input v-model="editingFile.vFolder" placeholder="Qovluğun adını yazın..." class="w-full p-4 bg-slate-50 rounded-2xl border border-blue-100 outline-none focus:ring-2 ring-blue-200">
                    </div>
                </div>

                <div class="flex gap-3 mt-auto">
                    <button @click="saveEdit" class="flex-1 bg-blue-600 text-white py-4 rounded-2xl font-bold hover:bg-blue-700 transition">Yadda Saxla</button>
                    <button @click="editingFile = null" class="px-8 py-4 bg-slate-100 text-slate-400 rounded-2xl font-bold hover:bg-slate-200 transition">Ləğv Et</button>
                </div>
            </div>
        </div>
    </div>

    <script>
        window.addEventListener("dragover", function(e) { e.preventDefault(); }, false);
        window.addEventListener("drop", function(e) { e.preventDefault(); }, false);

        const { createApp } = Vue
        createApp({
            data() {
                return {
                    query: '', 
                    searchResults: [],
                    recents: [], 
                    virtualFolders: [],
                    totalFiles: 0,
                    activeModal: null, 
                    editingFile: null, 
                    newTag: '',
                    dragActive: false, 
                    uploadingFile: null, 
                    uploadTags: [], 
                    uploadVFolder: '',
                    searchTimeout: null,
                    isSearching: false,
                    
                    noteTitle: '',
                    noteContent: '',
                    editTextId: null,
                    
                    offset: 0,
                    limit: 50,
                    hasMore: true,
                    isLoadingMore: false,
                    currentFolderFilter: '',
                    
                    // YENİ: UI Toast siyahısı
                    toasts: []
                }
            },
            methods: {
                // YENİ BİLDİRİŞ SİSTEMİ: İstənilən yerdə və IP-də ekranın üzərində göstərəcək!
                notifyUser(message) {
                    // 1. Həmişə ekranda qəşəng UI Toast çıxart
                    const id = Date.now();
                    this.toasts.push({ id, message });
                    
                    // 4 saniyə sonra avtomatik silinir
                    setTimeout(() => {
                        this.toasts = this.toasts.filter(t => t.id !== id);
                    }, 4000);

                    // 2. Əgər mümkündürsə (Lokalhost və ya icazə verilibsə) əlavə olaraq OS bildirişi də göndər
                    if ("Notification" in window) {
                        if (Notification.permission === "granted") {
                            new Notification("ArxivGo", { body: message });
                        }
                    }
                    
                    this.$nextTick(() => lucide.createIcons());
                },

                async fetchMeta() {
                    const res = await fetch('/api/meta');
                    const data = await res.json();
                    if(data) {
                        this.recents = data.recents || [];
                        this.virtualFolders = data.folders || [];
                        this.totalFiles = data.totalFiles || 0;
                    }
                },
                async performSearch(isAppend = false) {
                    if (!isAppend) {
                        this.offset = 0;
                        this.searchResults = [];
                        this.hasMore = true;
                    }

                    let url = '/api/search?limit=' + this.limit + '&offset=' + this.offset;
                    if (this.currentFolderFilter) {
                        url += '&folder=' + encodeURIComponent(this.currentFolderFilter);
                    } else if (this.query) {
                        url += '&q=' + encodeURIComponent(this.query);
                    } else {
                        return;
                    }

                    const res = await fetch(url);
                    const data = await res.json() || [];

                    if (isAppend) {
                        this.searchResults.push(...data);
                    } else {
                        this.searchResults = data;
                    }

                    if (data.length < this.limit) {
                        this.hasMore = false; 
                    }
                    this.offset += data.length;
                },
                onSearchInput() {
                    clearTimeout(this.searchTimeout);
                    this.currentFolderFilter = ''; 
                    
                    if (this.query.length === 0) {
                        this.searchResults = [];
                        this.isSearching = false;
                        return;
                    }
                    
                    this.isSearching = true;
                    this.searchTimeout = setTimeout(async () => {
                        await this.performSearch(false);
                        this.isSearching = false;
                    }, 300);
                },
                async searchFolder(folderName) {
                    this.activeModal = null;
                    this.query = "Qovluq: " + folderName;
                    this.currentFolderFilter = folderName;
                    this.isSearching = true;
                    await this.performSearch(false);
                    this.isSearching = false;
                },
                async handleScroll(e) {
                    const { scrollTop, clientHeight, scrollHeight } = e.target;
                    if (scrollTop + clientHeight >= scrollHeight - 20) {
                        if (this.hasMore && !this.isLoadingMore) {
                            this.isLoadingMore = true;
                            await this.performSearch(true);
                            this.isLoadingMore = false;
                        }
                    }
                },

                async openFile(f) {
                    const host = window.location.hostname;
                    const isLocal = (host === 'localhost' || host === '127.0.0.1' || host === '[::1]');
                    
                    const ext = f.name.slice((f.name.lastIndexOf(".") - 1 >>> 0) + 2).toLowerCase();
                    const isTextFile = ['txt', 'md', 'json', 'csv', 'log', 'html', 'css', 'js'].includes(ext);

                    if (!isLocal && isTextFile) {
                        const res = await fetch('/api/get-content?id=' + f.id);
                        if (res.ok) {
                            const content = await res.text();
                            this.editTextId = f.id;
                            this.noteTitle = f.name;
                            this.noteContent = content;
                            this.openModal('edit-text');
                            return;
                        }
                    }

                    if (isLocal) {
                        await fetch('/api/open?id=' + f.id);
                    } else {
                        window.open('/api/download?id=' + f.id, '_blank');
                    }
                    setTimeout(() => this.fetchMeta(), 1000);
                },

                downloadCurrentTextFile() {
                    if (this.editTextId) {
                        window.open('/api/download?id=' + this.editTextId, '_blank');
                    }
                },

                async saveTextContent() {
                    if (!this.noteContent) return;
                    const payload = { id: this.editTextId, content: this.noteContent };

                    const res = await fetch('/api/update-content', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(payload)
                    });
                    const data = await res.json();

                    if (data.versioned) {
                        this.notifyUser("YENİ VERSİYA yaradıldı: " + data.fileName);
                    } else {
                        this.notifyUser("Fayl redaktə edildi: " + data.fileName);
                    }

                    this.activeModal = null;
                    this.editTextId = null;
                    this.noteContent = '';
                    this.fetchMeta();
                },

                startEdit(file) {
                    this.editingFile = JSON.parse(JSON.stringify(file));
                    this.$nextTick(() => lucide.createIcons());
                },
                addTag() {
                    if (this.newTag && !this.editingFile.tags.includes(this.newTag)) {
                        this.editingFile.tags.push(this.newTag.toLowerCase().trim());
                        this.newTag = '';
                    }
                },
                removeTag(tag) { this.editingFile.tags = this.editingFile.tags.filter(t => t !== tag); },
                
                async saveEdit() {
                    const res = await fetch('/api/update', { method: 'POST', body: JSON.stringify(this.editingFile) });
                    const data = await res.json();
                    
                    if (data.moved) {
                        this.notifyUser("Fayl başqa qovluğa köçürüldü: " + data.fileName);
                    } else {
                        this.notifyUser("Məlumatlar yeniləndi: " + data.fileName);
                    }

                    this.editingFile = null;
                    this.fetchMeta();
                    if (this.query) await this.performSearch(false); 
                },
                
                onDragEnter(e) { e.preventDefault(); this.dragActive = true; },
                onDragOver(e) { e.preventDefault(); this.dragActive = true; },
                onDragLeave(e) { 
                    e.preventDefault(); 
                    if (!e.relatedTarget || e.relatedTarget.nodeName === "HTML") {
                        this.dragActive = false; 
                    }
                },
                onDrop(e) {
                    e.preventDefault();
                    this.dragActive = false;
                    
                    if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files.length > 0) {
                        this.uploadingFile = e.dataTransfer.files[0];
                        this.uploadTags = [];
                        this.uploadVFolder = '';
                        this.$nextTick(() => lucide.createIcons());
                    }
                },
                addUploadTag() {
                    if (this.newTag && !this.uploadTags.includes(this.newTag)) {
                        this.uploadTags.push(this.newTag.toLowerCase().trim());
                        this.newTag = '';
                    }
                },
                async confirmUpload() {
                    const formData = new FormData();
                    formData.append('file', this.uploadingFile);
                    formData.append('tags', JSON.stringify(this.uploadTags));
                    formData.append('vFolder', this.uploadVFolder);

                    const res = await fetch('/api/upload', { method: 'POST', body: formData });
                    const data = await res.json();
                    
                    if (data.versioned) {
                        this.notifyUser("YENİ VERSİYA əlavə edildi: " + data.fileName);
                    } else {
                        this.notifyUser("Fayl sistemə əlavə edildi: " + data.fileName);
                    }

                    this.uploadingFile = null;
                    this.fetchMeta();
                },
                async saveNote() {
                    if (!this.noteTitle.trim() || !this.noteContent.trim()) {
                        alert("Zəhmət olmasa, başlıq və məzmunu daxil edin.");
                        return;
                    }
                    const payload = {
                        title: this.noteTitle,
                        content: this.noteContent,
                        tags: this.uploadTags,
                        vFolder: this.uploadVFolder
                    };

                    const res = await fetch('/api/create-note', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(payload)
                    });
                    const data = await res.json();

                    if (data.versioned) {
                        this.notifyUser("Qeydin YENİ VERSİYASI yaradıldı: " + data.fileName);
                    } else {
                        this.notifyUser("Yeni qeyd yaradıldı: " + data.fileName);
                    }

                    this.activeModal = null;
                    this.fetchMeta();
                    if (this.query) await this.performSearch(false);
                },
                openModal(type) {
                    this.activeModal = type;
                    if (type === 'note' || type === 'upload') {
                        this.uploadTags = [];
                        this.uploadVFolder = '';
                        this.noteTitle = '';
                        this.noteContent = '';
                        this.newTag = '';
                    }
                    this.$nextTick(() => lucide.createIcons());
                },
                formatDate(d) { 
                    const date = new Date(d);
                    const now = new Date();
                    if(date.toDateString() === now.toDateString()){
                         return date.toLocaleTimeString([], {hour: '2-digit', minute:'2-digit'});
                    }
                    return date.toLocaleDateString(); 
                }
            },
            mounted() {
                this.fetchMeta();
                lucide.createIcons();
                setInterval(this.fetchMeta, 5000);

                window.addEventListener('dragenter', this.onDragEnter);
                window.addEventListener('dragover', this.onDragOver);
                window.addEventListener('dragleave', this.onDragLeave);
                window.addEventListener('drop', this.onDrop);

                // Təhlükəsizlik icazəsi üçün klik hadisəsi
                const requestPerm = () => {
                    if ("Notification" in window && Notification.permission === "default" && window.isSecureContext) {
                        Notification.requestPermission();
                    }
                    window.removeEventListener('click', requestPerm);
                };
                window.addEventListener('click', requestPerm);
            },
            beforeUnmount() {
                window.removeEventListener('dragenter', this.onDragEnter);
                window.removeEventListener('dragover', this.onDragOver);
                window.removeEventListener('dragleave', this.onDragLeave);
                window.removeEventListener('drop', this.onDrop);
            },
            updated() { lucide.createIcons(); }
        }).mount('#app')
    </script>
</body>
</html>
`