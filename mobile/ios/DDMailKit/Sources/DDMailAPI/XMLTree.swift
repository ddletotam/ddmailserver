import Foundation

/// Маленькое дерево XML (автонастройка, ответы DAV) поверх `XMLParser`.
public final class XMLTree: @unchecked Sendable {
    public let name: String
    public let attrs: [String: String]
    public private(set) var children: [XMLTree] = []
    public private(set) var text = ""

    init(name: String, attrs: [String: String]) {
        self.name = name
        self.attrs = attrs
    }

    /// Разобрать документ; имена — без префикса пространства имён.
    public static func parse(_ xml: String) -> XMLTree? {
        let b = Builder()
        let p = XMLParser(data: Data(xml.utf8))
        p.shouldProcessNamespaces = true
        p.delegate = b
        guard p.parse(), let root = b.root else { return nil }
        return root
    }

    public func child(_ tag: String) -> String? {
        children.first { $0.name == tag }?.text.trimmingCharacters(in: .whitespacesAndNewlines)
    }

    public func first(_ tag: String) -> XMLTree? { children.first { $0.name == tag } }

    public func descendants(_ tag: String) -> [XMLTree] {
        var out: [XMLTree] = []
        for c in children {
            if c.name == tag { out.append(c) }
            out += c.descendants(tag)
        }
        return out
    }

    private final class Builder: NSObject, XMLParserDelegate {
        var root: XMLTree?
        var stack: [XMLTree] = []

        func parser(_ parser: XMLParser, didStartElement elementName: String, namespaceURI: String?, qualifiedName qName: String?, attributes: [String: String] = [:]) {
            let n = XMLTree(name: elementName, attrs: attributes)
            stack.last?.children.append(n)
            if root == nil { root = n }
            stack.append(n)
        }

        func parser(_ parser: XMLParser, didEndElement elementName: String, namespaceURI: String?, qualifiedName qName: String?) {
            _ = stack.popLast()
        }

        func parser(_ parser: XMLParser, foundCharacters string: String) {
            stack.last?.text += string
        }

        func parser(_ parser: XMLParser, foundCDATA CDATABlock: Data) {
            stack.last?.text += String(decoding: CDATABlock, as: UTF8.self)
        }
    }
}
