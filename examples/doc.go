// Package examples holds the neferclient and NeferGUI example programs
// (layer, lock) and their headless tests. The two libraries never import each
// other: each program copies plain fields between them. The layer program also
// shows the input region: NeferGUI's input rectangles become the Wayland input
// region, so pointer input outside the button passes through the surface.
package examples
