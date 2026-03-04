package main

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

// YENİ: Qeyd yaratmaq üçün model
type NoteRequest struct {
	Title   string   `json:"title"`
	Content string   `json:"content"`
	Tags    []string `json:"tags"`
	VFolder string   `json:"vFolder"`
}

const dbPath = "data.json"
const storageFolder = "ArxivGo_Storage"
var state AppState

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

// --- SÜRƏTLİ SKAN VƏ UNİKAL ID YARADILMASI ---

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
	limit := 100 
	
	var results []FileData
	
	state.mu.RLock()
	defer state.mu.RUnlock()

	for _, f := range state.Files {
		if folderFilter != "" && f.VFolder != folderFilter {
			continue
		}

		if query != "" {
			match := strings.Contains(strings.ToLower(f.Name), query)
			if !match {
				for _, t := range f.Tags {
					if strings.Contains(strings.ToLower(t), query) {
						match = true
						break
					}
				}
			}
			if !match { continue }
		}

		results = append(results, f)
		if len(results) >= limit { break }
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
	state.mu.Lock()
	for i, f := range state.Files {
		if f.ID == updated.ID {
			state.Files[i].Tags = updated.Tags
			state.Files[i].VFolder = updated.VFolder
			break
		}
	}
	state.mu.Unlock()
	go saveDB() 
	w.WriteHeader(http.StatusOK)
}

func uploadHandler(w http.ResponseWriter, r *http.Request) {
	err := r.ParseMultipartForm(500 << 20)
	if err != nil { http.Error(w, err.Error(), 400); return }

	file, handler, err := r.FormFile("file")
	if err != nil { http.Error(w, err.Error(), 400); return }
	defer file.Close()

	tagsJSON := r.FormValue("tags")
	vFolder := r.FormValue("vFolder")

	destPath := filepath.Join(storageFolder, handler.Filename)
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

	newFile := FileData{
		ID:        uniqueID,
		Name:      handler.Filename,
		Path:      absPath,
		VFolder:   vFolder,
		Tags:      tags,
		CreatedAt: time.Now(),
	}

	state.mu.Lock()
	state.Files = append(state.Files, newFile)
	state.mu.Unlock()
	go saveDB()

	w.WriteHeader(http.StatusOK)
}

// YENİ: Qeyd Yaratma (Create Note) Handleri
func createNoteHandler(w http.ResponseWriter, r *http.Request) {
	var req NoteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	// Fayl adı üçün başlığı təmizləyirik (boşluqları alt xətt ilə əvəz edirik)
	safeTitle := strings.ReplaceAll(req.Title, " ", "_")
	safeTitle = strings.ReplaceAll(safeTitle, "/", "-")
	safeTitle = strings.ReplaceAll(safeTitle, "\\", "-")
	
	if safeTitle == "" {
		safeTitle = "Adsiz_Qeyd"
	}
	
	fileName := safeTitle + ".txt"
	destPath := filepath.Join(storageFolder, fileName)

	// Əgər eyni adlı qeyd varsa, sonuna vaxt (timestamp) artırırıq
	if _, err := os.Stat(destPath); err == nil {
		fileName = fmt.Sprintf("%s_%d.txt", safeTitle, time.Now().Unix())
		destPath = filepath.Join(storageFolder, fileName)
	}

	// .txt faylını fiziki olaraq Storage qovluğuna yazırıq
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

	w.WriteHeader(http.StatusOK)
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
			exec.Command("rundll32", "url.dll,FileProtocolHandler", targetPath).Start()
			fmt.Fprint(w, "Opened on Windows")
		case "darwin":  
			exec.Command("open", targetPath).Start()
			fmt.Fprint(w, "Opened on Mac")
		default:        
			http.ServeFile(w, r, targetPath)
		}
	} else {
		http.Error(w, "Fayl tapılmadı", http.StatusNotFound)
	}
}

// --- MAIN ---

func main() {
	initStorage()
	loadDB()
	go autoStartupScan()

	http.HandleFunc("/api/search", searchHandler) 
	http.HandleFunc("/api/meta", metaHandler)     
	http.HandleFunc("/api/update", updateHandler)
	http.HandleFunc("/api/upload", uploadHandler)
	http.HandleFunc("/api/create-note", createNoteHandler) // YENİ: Note API-si
	http.HandleFunc("/api/scan", scanHandler)
	http.HandleFunc("/api/open", openFileHandler)

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, uiHTML)
	})

	fmt.Println("🚀 Server 8080 portunda hazırdır. UI: http://localhost:8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// --- UI (FRONTEND) ---
const uiHTML = `
<!DOCTYPE html>
<html lang="az">
<head>
    <meta charset="UTF-8">
    <title>ArxivGo | Performance Edition</title>
    <script src="https://cdn.tailwindcss.com"></script>
    <script src="https://unpkg.com/vue@3/dist/vue.global.js"></script>
    <script src="https://unpkg.com/lucide@latest"></script>
    <style>
        @import url('https://fonts.googleapis.com/css2?family=Inter:wght@300;400;600&display=swap');
        body { font-family: 'Inter', sans-serif; background-color: #fff; color: #1e293b; margin:0; padding:0; overflow: hidden; }
        .search-container:focus-within { ring: 4px solid #eff6ff; border-color: #3b82f6; }
        .modal-overlay { background: rgba(255, 255, 255, 0.98); backdrop-filter: blur(10px); z-index: 100; }
        .custom-scroll::-webkit-scrollbar { width: 4px; }
        .custom-scroll::-webkit-scrollbar-thumb { background: #e2e8f0; border-radius: 10px; }
        .drop-zone-active { z-index: 9999; opacity: 1; pointer-events: all; }
        .drop-zone-inactive { z-index: -1; opacity: 0; pointer-events: none; }
    </style>
</head>
<body class="w-screen h-screen">
    
    <div id="app" class="w-full h-full flex items-center justify-center relative bg-white">
        
        <div :class="dragActive ? 'drop-zone-active' : 'drop-zone-inactive'" 
             class="fixed inset-0 bg-blue-50/90 border-[6px] border-dashed border-blue-400 flex items-center justify-center transition-opacity duration-200">
            <div class="text-3xl font-bold text-blue-600 bg-white px-10 py-6 rounded-3xl shadow-2xl flex items-center gap-4">
                <i data-lucide="upload-cloud" class="w-12 h-12"></i> Faylı buraya buraxın
            </div>
        </div>

        <div class="w-full max-w-2xl px-6 relative z-10">
            <div v-if="!activeModal && !editingFile && !uploadingFile" class="text-center">
                <h1 class="text-5xl font-light mb-1 select-none">Arxiv<span class="font-bold text-blue-600">Go</span></h1>
                <p class="text-[10px] text-slate-400 font-bold uppercase tracking-widest mb-10">Cəmi Fayl: {{ totalFiles }}</p>

                <div class="search-container flex items-center bg-white border border-slate-200 rounded-full px-6 py-4 mb-8 transition-all shadow-sm">
                    <i data-lucide="search" class="w-5 h-5 text-slate-400 mr-4"></i>
                    <input v-model="query" @input="onSearchInput" type="text" placeholder="Axtar və ya faylı ekrana at..." class="flex-1 outline-none text-lg bg-transparent">
                    <div v-if="isSearching" class="w-4 h-4 border-2 border-blue-500 border-t-transparent rounded-full animate-spin"></div>
                </div>

                <div v-if="searchResults.length > 0" class="absolute w-full left-0 max-w-2xl mx-auto mt-[-20px] bg-white border border-slate-100 rounded-3xl shadow-2xl z-50 overflow-hidden text-left max-h-[60vh] custom-scroll overflow-y-auto">
                    <div v-for="f in searchResults" :key="f.id" class="px-6 py-4 hover:bg-blue-50/50 flex justify-between items-center group cursor-pointer border-b border-slate-50 last:border-0">
                        <div @click="openFile(f.id)" class="flex-1 flex items-center gap-4">
                            <i data-lucide="file" class="w-5 h-5 text-slate-300"></i>
                            <div>
                                <div class="text-sm font-medium">{{ f.name }}</div>
                                <div class="flex gap-2 mt-1">
                                    <span v-for="t in f.tags" class="text-[9px] bg-blue-50 text-blue-500 px-2 py-0.5 rounded font-bold uppercase tracking-tighter">#{{ t }}</span>
                                    <span v-if="f.vFolder" class="text-[9px] bg-slate-100 text-slate-500 px-2 py-0.5 rounded font-bold uppercase tracking-tighter">{{ f.vFolder }}</span>
                                </div>
                            </div>
                        </div>
                        <button @click.stop="startEdit(f)" class="p-2 opacity-0 group-hover:opacity-100 hover:bg-white shadow-sm rounded-full transition text-blue-600">
                            <i data-lucide="edit-3" class="w-4 h-4"></i>
                        </button>
                    </div>
                </div>

                <div class="flex justify-center gap-3">
                    <button @click="openModal('folders')" class="px-5 py-2 rounded-xl bg-slate-50 text-slate-600 text-xs font-bold hover:bg-slate-100 transition uppercase tracking-widest border border-slate-100">Virtual Qovluqlar</button>
                    <button @click="openModal('recents')" class="px-5 py-2 rounded-xl bg-slate-50 text-slate-600 text-xs font-bold hover:bg-slate-100 transition uppercase tracking-widest border border-slate-100">Son Əlavələr</button>
                    <button @click="openModal('note')" class="px-5 py-2 rounded-xl bg-green-50 text-green-600 text-xs font-bold hover:bg-green-100 transition uppercase tracking-widest border border-green-100 flex items-center gap-2"><i data-lucide="plus-circle" class="w-4 h-4"></i> Yeni Qeyd</button>
                </div>
            </div>
        </div>

        <div v-if="uploadingFile" class="fixed inset-0 modal-overlay flex items-center justify-center p-6 z-[1000]">
            <div class="w-full max-w-md bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10">
                <div class="flex items-center gap-3 text-blue-600 mb-2">
                    <i data-lucide="file-plus" class="w-6 h-6"></i>
                    <h3 class="text-xl font-bold truncate">{{ uploadingFile.name }}</h3>
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
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Virtual Qovluq</label>
                    <input v-model="uploadVFolder" list="folder-list" class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-blue-100">
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
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Teqlər (Enter ilə əlavə et)</label>
                        <div class="flex flex-wrap gap-2 p-3 bg-slate-50 rounded-2xl border border-slate-100">
                            <span v-for="t in uploadTags" @click="uploadTags = uploadTags.filter(tag => tag !== t)" class="bg-white text-green-600 px-3 py-1 rounded-full text-xs font-bold border border-green-100 cursor-pointer hover:bg-red-50 hover:text-red-500 hover:border-red-100 transition">#{{ t }}</span>
                            <input v-model="newTag" @keyup.enter="addUploadTag" placeholder="..." class="bg-transparent outline-none text-xs flex-1">
                        </div>
                    </div>

                    <div class="mb-6">
                        <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Virtual Qovluq</label>
                        <input v-model="uploadVFolder" list="folder-list" class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-green-100">
                    </div>
                </div>

                <div class="flex gap-3 mt-4">
                    <button @click="saveNote" class="flex-1 bg-green-600 text-white py-4 rounded-2xl font-bold hover:bg-green-700 transition">Qeydi Yarat</button>
                    <button @click="activeModal = null" class="px-8 py-4 bg-slate-100 text-slate-400 rounded-2xl font-bold hover:bg-slate-200 transition">Ləğv Et</button>
                </div>
            </div>
        </div>

        <div v-if="(activeModal === 'folders' || activeModal === 'recents') && !editingFile && !uploadingFile" class="fixed inset-0 modal-overlay flex items-center justify-center p-6">
            <div class="w-full max-w-xl bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10 flex flex-col max-h-[80vh]">
                <div class="flex justify-between items-center mb-8">
                    <h2 class="text-2xl font-bold capitalize">{{ activeModal === 'folders' ? 'Qovluqlar' : 'Son Əlavələr' }}</h2>
                    <button @click="activeModal = null" class="p-2 hover:bg-slate-100 rounded-full transition"><i data-lucide="x"></i></button>
                </div>
                <div class="flex-1 overflow-y-auto custom-scroll">
                    <div v-if="activeModal === 'folders'" class="grid grid-cols-2 gap-3">
                        <div v-for="v in virtualFolders" @click="searchFolder(v)" class="p-5 bg-slate-50 rounded-2xl border border-slate-100 hover:border-blue-400 hover:bg-blue-50 cursor-pointer transition">
                            <i data-lucide="folder" class="w-5 h-5 text-blue-500 mb-2"></i>
                            <div class="text-sm font-bold text-slate-700">{{ v || 'Adsız' }}</div>
                        </div>
                    </div>
                    <div v-if="activeModal === 'recents'" class="space-y-3">
                        <div v-for="f in recents" @click="openFile(f.id)" class="p-4 bg-slate-50 rounded-2xl flex justify-between items-center cursor-pointer hover:bg-blue-50 transition">
                            <span class="text-sm font-medium truncate w-64">{{ f.name }}</span>
                            <span class="text-[10px] text-slate-400 font-bold uppercase">{{ formatDate(f.createdAt) }}</span>
                        </div>
                    </div>
                </div>
            </div>
        </div>

        <div v-if="editingFile" class="fixed inset-0 modal-overlay flex items-center justify-center p-6">
            <div class="w-full max-w-md bg-white rounded-[40px] shadow-2xl border border-slate-100 p-10">
                <h3 class="text-xl font-bold mb-2 truncate">{{ editingFile.name }}</h3>
                <p class="text-[10px] text-slate-400 uppercase tracking-widest mb-8">Fayl Redaktəsi</p>

                <div class="mb-6">
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Teqlər</label>
                    <div class="flex flex-wrap gap-2 p-3 bg-slate-50 rounded-2xl border border-slate-100">
                        <span v-for="t in editingFile.tags" @click="removeTag(t)" class="bg-white text-blue-600 px-3 py-1 rounded-full text-xs font-bold border border-blue-100 cursor-pointer hover:bg-red-50 hover:text-red-500 hover:border-red-100 transition">#{{ t }}</span>
                        <input v-model="newTag" @keyup.enter="addTag" placeholder="..." class="bg-transparent outline-none text-xs flex-1">
                    </div>
                </div>

                <div class="mb-10">
                    <label class="text-[10px] font-bold text-slate-400 uppercase block mb-2">Virtual Qovluq</label>
                    <input v-model="editingFile.vFolder" list="folder-list" class="w-full p-4 bg-slate-50 rounded-2xl border-none outline-none focus:ring-2 ring-blue-100">
                    <datalist id="folder-list"><option v-for="v in virtualFolders" :value="v"></datalist>
                </div>

                <div class="flex gap-3">
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
                    // Qeyd yaratmaq üçün yeni dəyişənlər
                    noteTitle: '',
                    noteContent: ''
                }
            },
            methods: {
                async fetchMeta() {
                    const res = await fetch('/api/meta');
                    const data = await res.json();
                    if(data) {
                        this.recents = data.recents || [];
                        this.virtualFolders = data.folders || [];
                        this.totalFiles = data.totalFiles || 0;
                    }
                },
                onSearchInput() {
                    clearTimeout(this.searchTimeout);
                    if (this.query.length === 0) {
                        this.searchResults = [];
                        this.isSearching = false;
                        return;
                    }
                    this.isSearching = true;
                    this.searchTimeout = setTimeout(async () => {
                        const res = await fetch('/api/search?q=' + encodeURIComponent(this.query));
                        this.searchResults = await res.json() || [];
                        this.isSearching = false;
                    }, 300);
                },
                async searchFolder(folderName) {
                    this.activeModal = null;
                    this.query = "Qovluq: " + folderName;
                    this.isSearching = true;
                    const res = await fetch('/api/search?folder=' + encodeURIComponent(folderName));
                    this.searchResults = await res.json() || [];
                    this.isSearching = false;
                },
                async openFile(id) {
                    window.open('/api/open?id=' + id, '_blank');
                    setTimeout(() => this.fetchMeta(), 1000);
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
                    await fetch('/api/update', { method: 'POST', body: JSON.stringify(this.editingFile) });
                    this.editingFile = null;
                    this.fetchMeta();
                    this.onSearchInput(); 
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

                    await fetch('/api/upload', { method: 'POST', body: formData });
                    
                    this.uploadingFile = null;
                    this.fetchMeta();
                },
                // YENİ: Qeyd Yaratma Funksiyası
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

                    await fetch('/api/create-note', {
                        method: 'POST',
                        headers: { 'Content-Type': 'application/json' },
                        body: JSON.stringify(payload)
                    });

                    this.activeModal = null;
                    this.fetchMeta();
                    if (this.query) this.onSearchInput();
                },
                openModal(type) {
                    this.activeModal = type;
                    // Yeni qeyd və ya fayl yükləmə paneli açıldıqda inputları təmizlə
                    if (type === 'note' || type === 'upload') {
                        this.uploadTags = [];
                        this.uploadVFolder = '';
                        this.noteTitle = '';
                        this.noteContent = '';
                        this.newTag = '';
                    }
                    this.$nextTick(() => lucide.createIcons());
                },
                formatDate(d) { return new Date(d).toLocaleDateString(); }
            },
            mounted() {
                this.fetchMeta();
                lucide.createIcons();
                setInterval(this.fetchMeta, 5000);

                window.addEventListener('dragenter', this.onDragEnter);
                window.addEventListener('dragover', this.onDragOver);
                window.addEventListener('dragleave', this.onDragLeave);
                window.addEventListener('drop', this.onDrop);
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
