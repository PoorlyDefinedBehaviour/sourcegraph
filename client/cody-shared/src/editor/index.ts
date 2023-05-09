export interface ActiveTextEditor {
    content: string
    filePath: string
}

export interface ActiveTextEditorSelection {
    fileName: string
    precedingText: string
    selectedText: string
    followingText: string

    selectedRange: ActiveTextEditorSelectionRange | null
}

export interface ActiveTextEditorSelectionRange {
    /**
     * 0-based line number for the start of the selected text.
     */
    lineStart: number

    /**
     * 0-based line number for the start of the selected text.
     */
    lineEnd: number
}

export interface ActiveTextEditorVisibleContent {
    content: string
    fileName: string
}

export interface Editor {
    getWorkspaceRootPath(): string | null
    getActiveTextEditor(): ActiveTextEditor | null
    getActiveTextEditorSelection(): ActiveTextEditorSelection | null

    /**
     * Gets the active text editor's selection, or the entire file if the selected range is empty.
     */
    getActiveTextEditorSelectionOrEntireFile(): ActiveTextEditorSelection | null

    getActiveTextEditorVisibleContent(): ActiveTextEditorVisibleContent | null
    replaceSelection(fileName: string, selectedText: string, replacement: string): Promise<void>
    showQuickPick(labels: string[]): Promise<string | undefined>
    showWarningMessage(message: string): Promise<void>
    showInputBox(prompt?: string): Promise<string | undefined>
}
